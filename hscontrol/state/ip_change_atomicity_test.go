package state

import (
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangeIPv4AddressesNode_Atomicity(t *testing.T) {
	prefixV4 := netip.MustParsePrefix("100.64.0.0/10")
	prefixV6 := netip.MustParsePrefix("fd7a:1122:3344::/48")

	dbPath := fmt.Sprintf("/tmp/headscale_test_atomicity_%d.db", time.Now().UnixNano())
	cfg := &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabaseSqlite,
			Sqlite: types.SqliteConfig{
				Path: dbPath,
			},
		},
		PrefixV4: &prefixV4,
		PrefixV6: &prefixV6,
	}

	state, err := NewState(cfg)
	require.NoError(t, err)
	defer state.Close()
	defer os.Remove(dbPath)

	user1 := state.CreateUserForTest("user1")
	node := state.CreateRegisteredNodeForTest(user1, "testnode")
	state.nodeStore.PutNode(*node)
	if node.IPv4 != nil {
		state.ipAlloc.AddIP(*node.IPv4)
	}
	if node.IPv6 != nil {
		state.ipAlloc.AddIP(*node.IPv6)
	}
	nodeID := node.ID

	require.NotNil(t, node.IPv4, "Node should have an IPv4 address")
	oldIPv4 := *node.IPv4
	newIPv4 := netip.MustParseAddr("100.64.0.254")

	_, _, err = state.ChangeIPv4AddressesNode(nodeID, newIPv4.String())
	require.NoError(t, err)

	updatedNode, ok := state.GetNodeByID(nodeID)
	require.True(t, ok, "Node should be found in NodeStore after successful change")
	require.NotNil(t, updatedNode.IPv4(), "Updated node should have an IPv4 address")
	assert.Equal(t, newIPv4, updatedNode.IPv4().Get())

	_, err = state.ipAlloc.IsAvailableIP(newIPv4)
	assert.Error(t, err, "New IP should not be available (it's taken)")

	isAvailableOld, err := state.ipAlloc.IsAvailableIP(oldIPv4)
	require.NoError(t, err)
	assert.True(t, isAvailableOld, "Old IP should be freed")

	user2 := state.CreateUserForTest("user2")
	otherNode := state.CreateRegisteredNodeForTest(user2, "othernode")
	state.nodeStore.PutNode(*otherNode)
	if otherNode.IPv4 != nil {
		state.ipAlloc.AddIP(*otherNode.IPv4)
	}
	if otherNode.IPv6 != nil {
		state.ipAlloc.AddIP(*otherNode.IPv6)
	}
	require.NotNil(t, otherNode.IPv4, "Other node should have an IPv4 address")
	takenIP := *otherNode.IPv4

	_, _, err = state.ChangeIPv4AddressesNode(nodeID, takenIP.String())
	assert.Error(t, err)

	nodeAfterFail, ok := state.GetNodeByID(nodeID)
	require.True(t, ok, "Node should still be found in NodeStore after failed attempt")
	require.NotNil(t, nodeAfterFail.IPv4(), "Node should still have an IPv4 address after failed attempt")
	assert.Equal(t, newIPv4, nodeAfterFail.IPv4().Get(), "Node IP should not have changed after failed attempt")
}

func TestChangeIPv4AddressesNode_NilSafety(t *testing.T) {
	prefixV4 := netip.MustParsePrefix("100.64.0.0/10")
	prefixV6 := netip.MustParsePrefix("fd7a:1122:3344::/48")

	dbPath := fmt.Sprintf("/tmp/headscale_test_nil_%d.db", time.Now().UnixNano())
	cfg := &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabaseSqlite,
			Sqlite: types.SqliteConfig{
				Path: dbPath,
			},
		},
		PrefixV4: &prefixV4,
		PrefixV6: &prefixV6,
	}

	state, err := NewState(cfg)
	require.NoError(t, err)
	defer state.Close()
	defer os.Remove(dbPath)

	user := state.CreateUserForTest("testuser")
	node := state.CreateRegisteredNodeForTest(user, "testnode")
	node.IPv4 = nil

	state.nodeStore.PutNode(*node)

	newIPv4 := netip.MustParseAddr("100.64.0.2")

	_, _, err = state.ChangeIPv4AddressesNode(node.ID, newIPv4.String())
	if err != nil {
		t.Logf("Function returned error as expected: %v", err)
	}
}
