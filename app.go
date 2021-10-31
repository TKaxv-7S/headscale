package headscale

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/patrickmn/go-cache"
	"golang.org/x/oauth2"

	"github.com/gin-gonic/gin"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	apiV1 "github.com/juanfont/headscale/gen/go/v1"
	"github.com/rs/zerolog/log"
	"github.com/soheilhy/cmux"
	ginprometheus "github.com/zsais/go-gin-prometheus"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"gorm.io/gorm"
	"inet.af/netaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/types/dnstype"
	"tailscale.com/types/wgkey"
)

// Config contains the initial Headscale configuration.
type Config struct {
	ServerURL                      string
	Addr                           string
	PrivateKeyPath                 string
	EphemeralNodeInactivityTimeout time.Duration
	IPPrefix                       netaddr.IPPrefix
	BaseDomain                     string

	DERP DERPConfig

	DBtype string
	DBpath string
	DBhost string
	DBport int
	DBname string
	DBuser string
	DBpass string

	TLSLetsEncryptListen        string
	TLSLetsEncryptHostname      string
	TLSLetsEncryptCacheDir      string
	TLSLetsEncryptChallengeType string

	TLSCertPath string
	TLSKeyPath  string

	ACMEURL   string
	ACMEEmail string

	DNSConfig *tailcfg.DNSConfig

	OIDC OIDCConfig

	MaxMachineRegistrationDuration     time.Duration
	DefaultMachineRegistrationDuration time.Duration
}

type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	MatchMap     map[string]string
}

type DERPConfig struct {
	URLs            []url.URL
	Paths           []string
	AutoUpdate      bool
	UpdateFrequency time.Duration
}

// Headscale represents the base app of the service.
type Headscale struct {
	cfg        Config
	db         *gorm.DB
	dbString   string
	dbType     string
	dbDebug    bool
	publicKey  *wgkey.Key
	privateKey *wgkey.Private

	DERPMap *tailcfg.DERPMap

	aclPolicy *ACLPolicy
	aclRules  *[]tailcfg.FilterRule

	lastStateChange sync.Map

	oidcProvider   *oidc.Provider
	oauth2Config   *oauth2.Config
	oidcStateCache *cache.Cache
}

// NewHeadscale returns the Headscale app.
func NewHeadscale(cfg Config) (*Headscale, error) {
	content, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}

	privKey, err := wgkey.ParsePrivate(string(content))
	if err != nil {
		return nil, err
	}
	pubKey := privKey.Public()

	var dbString string
	switch cfg.DBtype {
	case "postgres":
		dbString = fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=disable", cfg.DBhost,
			cfg.DBport, cfg.DBname, cfg.DBuser, cfg.DBpass)
	case "sqlite3":
		dbString = cfg.DBpath
	default:
		return nil, errors.New("unsupported DB")
	}

	h := Headscale{
		cfg:        cfg,
		dbType:     cfg.DBtype,
		dbString:   dbString,
		privateKey: privKey,
		publicKey:  &pubKey,
		aclRules:   &tailcfg.FilterAllowAll, // default allowall
	}

	err = h.initDB()
	if err != nil {
		return nil, err
	}

	if cfg.OIDC.Issuer != "" {
		err = h.initOIDC()
		if err != nil {
			return nil, err
		}
	}

	if h.cfg.DNSConfig != nil && h.cfg.DNSConfig.Proxied { // if MagicDNS
		magicDNSDomains, err := generateMagicDNSRootDomains(h.cfg.IPPrefix, h.cfg.BaseDomain)
		if err != nil {
			return nil, err
		}
		// we might have routes already from Split DNS
		if h.cfg.DNSConfig.Routes == nil {
			h.cfg.DNSConfig.Routes = make(map[string][]dnstype.Resolver)
		}
		for _, d := range magicDNSDomains {
			h.cfg.DNSConfig.Routes[d.WithoutTrailingDot()] = nil
		}
	}

	return &h, nil
}

// Redirect to our TLS url.
func (h *Headscale) redirect(w http.ResponseWriter, req *http.Request) {
	target := h.cfg.ServerURL + req.URL.RequestURI()
	http.Redirect(w, req, target, http.StatusFound)
}

// expireEphemeralNodes deletes ephemeral machine records that have not been
// seen for longer than h.cfg.EphemeralNodeInactivityTimeout.
func (h *Headscale) expireEphemeralNodes(milliSeconds int64) {
	ticker := time.NewTicker(time.Duration(milliSeconds) * time.Millisecond)
	for range ticker.C {
		h.expireEphemeralNodesWorker()
	}
}

func (h *Headscale) expireEphemeralNodesWorker() {
	namespaces, err := h.ListNamespaces()
	if err != nil {
		log.Error().Err(err).Msg("Error listing namespaces")

		return
	}

	for _, ns := range *namespaces {
		machines, err := h.ListMachinesInNamespace(ns.Name)
		if err != nil {
			log.Error().Err(err).Str("namespace", ns.Name).Msg("Error listing machines in namespace")

			return
		}

		for _, m := range *machines {
			if m.AuthKey != nil && m.LastSeen != nil && m.AuthKey.Ephemeral &&
				time.Now().After(m.LastSeen.Add(h.cfg.EphemeralNodeInactivityTimeout)) {
				log.Info().Str("machine", m.Name).Msg("Ephemeral client removed from database")

				err = h.db.Unscoped().Delete(m).Error
				if err != nil {
					log.Error().
						Err(err).
						Str("machine", m.Name).
						Msg("🤮 Cannot delete ephemeral machine from the database")
				}
			}
		}

		h.setLastStateChangeToNow(ns.Name)
	}
}

// WatchForKVUpdates checks the KV DB table for requests to perform tailnet upgrades
// This is a way to communitate the CLI with the headscale server.
func (h *Headscale) watchForKVUpdates(milliSeconds int64) {
	ticker := time.NewTicker(time.Duration(milliSeconds) * time.Millisecond)
	for range ticker.C {
		h.watchForKVUpdatesWorker()
	}
}

func (h *Headscale) watchForKVUpdatesWorker() {
	h.checkForNamespacesPendingUpdates()
	// more functions will come here in the future
}

// Serve launches a GIN server with the Headscale API.
func (h *Headscale) Serve() error {
	var err error

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	defer cancel()

	l, err := net.Listen("tcp", h.cfg.Addr)
	if err != nil {
		panic(err)
	}

	// Create the cmux object that will multiplex 2 protocols on the same port.
	// The two following listeners will be served on the same port below gracefully.
	m := cmux.New(l)
	// Match gRPC requests here
	grpcListener := m.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	// Otherwise match regular http requests.
	httpListener := m.Match(cmux.Any())

	// Now create the grpc server with those options.
	grpcServer := grpc.NewServer()

	// TODO(kradalby): register the new server when we have authentication ready
	// apiV1.RegisterHeadscaleServiceServer(grpcServer, newHeadscaleV1APIServer(h))

	grpcGatewayMux := runtime.NewServeMux()

	opts := []grpc.DialOption{grpc.WithInsecure()}

	err = apiV1.RegisterHeadscaleServiceHandlerFromEndpoint(ctx, grpcGatewayMux, h.cfg.Addr, opts)
	if err != nil {
		return err
	}

	r := gin.Default()

	p := ginprometheus.NewPrometheus("gin")
	p.Use(r)

	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"healthy": "ok"}) })
	r.GET("/key", h.KeyHandler)
	r.GET("/register", h.RegisterWebAPI)
	r.POST("/machine/:id/map", h.PollNetMapHandler)
	r.POST("/machine/:id", h.RegistrationHandler)
	r.GET("/oidc/register/:mkey", h.RegisterOIDC)
	r.GET("/oidc/callback", h.OIDCCallback)
	r.GET("/apple", h.AppleMobileConfig)
	r.GET("/apple/:platform", h.ApplePlatformConfig)

	r.Any("/api/v1/*any", gin.WrapF(grpcGatewayMux.ServeHTTP))
	r.StaticFile("/swagger/swagger.json", "gen/openapiv2/v1/headscale.swagger.json")

	updateMillisecondsWait := int64(5000)

	// Fetch an initial DERP Map before we start serving
	h.DERPMap = GetDERPMap(h.cfg.DERP)

	if h.cfg.DERP.AutoUpdate {
		derpMapCancelChannel := make(chan struct{})
		defer func() { derpMapCancelChannel <- struct{}{} }()
		go h.scheduledDERPMapUpdateWorker(derpMapCancelChannel)
	}

	// I HATE THIS
	go h.watchForKVUpdates(updateMillisecondsWait)
	go h.expireEphemeralNodes(updateMillisecondsWait)

	httpServer := &http.Server{
		Addr:        h.cfg.Addr,
		Handler:     r,
		ReadTimeout: 30 * time.Second,
		// Go does not handle timeouts in HTTP very well, and there is
		// no good way to handle streaming timeouts, therefore we need to
		// keep this at unlimited and be careful to clean up connections
		// https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/#aboutstreaming
		WriteTimeout: 0,
	}

	tlsConfig, err := h.getTLSSettings()
	if err != nil {
		log.Error().Err(err).Msg("Failed to set up TLS configuration")

		return err
	}

	if tlsConfig != nil {
		httpServer.TLSConfig = tlsConfig
	}

	g := new(errgroup.Group)

	g.Go(func() error { return grpcServer.Serve(grpcListener) })
	g.Go(func() error { return httpServer.Serve(httpListener) })
	g.Go(func() error { return m.Serve() })

	log.Info().Msgf("listening and serving (multiplexed HTTP and gRPC) on: %s", h.cfg.Addr)

	return g.Wait()
}

func (h *Headscale) getTLSSettings() (*tls.Config, error) {
	if h.cfg.TLSLetsEncryptHostname != "" {
		if !strings.HasPrefix(h.cfg.ServerURL, "https://") {
			log.Warn().Msg("Listening with TLS but ServerURL does not start with https://")
		}

		m := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(h.cfg.TLSLetsEncryptHostname),
			Cache:      autocert.DirCache(h.cfg.TLSLetsEncryptCacheDir),
			Client: &acme.Client{
				DirectoryURL: h.cfg.ACMEURL,
			},
			Email: h.cfg.ACMEEmail,
		}

		if h.cfg.TLSLetsEncryptChallengeType == "TLS-ALPN-01" {
			// Configuration via autocert with TLS-ALPN-01 (https://tools.ietf.org/html/rfc8737)
			// The RFC requires that the validation is done on port 443; in other words, headscale
			// must be reachable on port 443.
			return m.TLSConfig(), nil
		} else if h.cfg.TLSLetsEncryptChallengeType == "HTTP-01" {
			// Configuration via autocert with HTTP-01. This requires listening on
			// port 80 for the certificate validation in addition to the headscale
			// service, which can be configured to run on any other port.
			go func() {
				log.Fatal().
					Err(http.ListenAndServe(h.cfg.TLSLetsEncryptListen, m.HTTPHandler(http.HandlerFunc(h.redirect)))).
					Msg("failed to set up a HTTP server")
			}()

			return m.TLSConfig(), nil
		} else {
			return nil, errors.New("unknown value for TLSLetsEncryptChallengeType")
		}
	} else if h.cfg.TLSCertPath == "" {
		if !strings.HasPrefix(h.cfg.ServerURL, "http://") {
			log.Warn().Msg("Listening without TLS but ServerURL does not start with http://")
		}

		return nil, nil
	} else {
		if !strings.HasPrefix(h.cfg.ServerURL, "https://") {
			log.Warn().Msg("Listening with TLS but ServerURL does not start with https://")
		}
		var err error
		tlsConfig := &tls.Config{}
		tlsConfig.ClientAuth = tls.RequireAnyClientCert
		tlsConfig.NextProtos = []string{"http/1.1"}
		tlsConfig.Certificates = make([]tls.Certificate, 1)
		tlsConfig.Certificates[0], err = tls.LoadX509KeyPair(h.cfg.TLSCertPath, h.cfg.TLSKeyPath)

		return tlsConfig, err
	}
}

func (h *Headscale) setLastStateChangeToNow(namespace string) {
	now := time.Now().UTC()
	lastStateUpdate.WithLabelValues("", "headscale").Set(float64(now.Unix()))
	h.lastStateChange.Store(namespace, now)
}

func (h *Headscale) getLastStateChange(namespaces ...string) time.Time {
	times := []time.Time{}

	for _, namespace := range namespaces {
		if wrapped, ok := h.lastStateChange.Load(namespace); ok {
			lastChange, _ := wrapped.(time.Time)

			times = append(times, lastChange)
		}
	}

	sort.Slice(times, func(i, j int) bool {
		return times[i].After(times[j])
	})

	log.Trace().Msgf("Latest times %#v", times)

	if len(times) == 0 {
		return time.Now().UTC()
	} else {
		return times[0]
	}
}
