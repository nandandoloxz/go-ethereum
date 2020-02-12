package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rpc"
)

type gethrpc struct {
	name     string
	rpc      *rpc.Client
	geth     *testgeth
	nodeInfo *p2p.NodeInfo
}

func (g *gethrpc) killAndWait() {
	g.geth.Logf("Killing %v", g.name)
	g.geth.Kill()
	g.geth.WaitExit()
}

func (g *gethrpc) callRPC(result interface{}, method string, args ...interface{}) {
	if err := g.rpc.Call(&result, method, args...); err != nil {
		g.geth.Fatalf("callRPC %v: %v", method, err)
	}
}

func (g *gethrpc) addPeer(peer *gethrpc, eventsToWait int) {
	g.geth.Logf("%v.addPeer(%v)", g.name, peer.name)
	enode := peer.getNodeInfo().Enode
	peerCh := make(chan *p2p.PeerEvent)
	sub, err := g.rpc.Subscribe(context.Background(), "admin", peerCh, "peerEvents")
	if err != nil {
		g.geth.Fatalf("subscribe %v: %v", g.name, err)
	}
	defer sub.Unsubscribe()
	g.callRPC(nil, "admin_addPeer", enode)
	timeout := time.After(14 * time.Second)
	for i := 0; i < eventsToWait; i++ {
		select {
		case ev := <-peerCh:
			g.geth.Logf("%v received event: type=%v, peer=%v", g.name, ev.Type, ev.Peer)
		case err := <-sub.Err():
			g.geth.Fatalf("%v sub error: %v", g.name, err)
			return
		case <-timeout:
			g.geth.Error("timeout adding peer")
			return
		}
	}
}

// Use this function instead of `g.nodeInfo` directly
func (g *gethrpc) getNodeInfo() *p2p.NodeInfo {
	if g.nodeInfo != nil {
		return g.nodeInfo
	}
	g.nodeInfo = &p2p.NodeInfo{}
	g.callRPC(&g.nodeInfo, "admin_nodeInfo")
	return g.nodeInfo
}

func (g *gethrpc) waitSynced() {
	// Check if it's synced now
	var result interface{}
	g.callRPC(&result, "eth_syncing")
	syncing, ok := result.(bool)
	if ok && !syncing {
		g.geth.Logf("%v already synced", g.name)
		return
	}

	// Actually wait, subscribe to the event
	ch := make(chan interface{})
	sub, err := g.rpc.Subscribe(context.Background(), "eth", ch, "syncing")
	if err != nil {
		g.geth.Fatalf("%v syncing: %v", g.name, err)
	}
	defer sub.Unsubscribe()
	g.geth.Log("subscribed")
	timeout := time.After(4 * time.Second)
	for {
		select {
		case ev := <-ch:
			g.geth.Log("'syncing' event", ev)
			syncing, ok := ev.(bool)
			if ok && !syncing {
				return
			}
			g.geth.Log("Other 'syncing' event", ev)
		case err := <-sub.Err():
			g.geth.Fatalf("%v notification: %v", g.name, err)
			return
		case <-timeout:
			g.geth.Fatalf("%v timeout syncing", g.name)
			return
		}
	}
}

func startGethWithRpc(t *testing.T, name string, args ...string) *gethrpc {
	g := &gethrpc{name: name}
	args = append([]string{"--networkid=42", "--port=0", "--nousb", "--rpc", "--rpcport=0", "--rpcapi=admin,eth,les"}, args...)
	g.geth = runGeth(t, args...)
	// wait before we can attach to it. TODO: probe for it properly
	time.Sleep(1 * time.Second)
	var err error
	ipcpath := filepath.Join(g.geth.Datadir, "geth.ipc")
	g.rpc, err = rpc.Dial(ipcpath)
	if err != nil {
		t.Fatalf("%v rpc connect: %v", name, err)
	}
	t.Logf("Started with rpc: %v", name)
	return g
}

func startServer(t *testing.T) *gethrpc {
	runGeth(t, "init", "./testdata/genesis.json").WaitExit()
	runGeth(t, "--gcmode=archive", "import", "./testdata/blockchain.blocks").WaitExit()
	g := startGethWithRpc(t, "server", "--nodiscover", "--light.serve=1", "--nat=extip:127.0.0.1")
	return g
}

func startLightServer(t *testing.T) *gethrpc {
	runGeth(t, "init", "./testdata/genesis.json").WaitExit()
	// Start it as a miner, otherwise it won't consider itself synced
	// Use the coinbase from testdata/genesis.json
	etherbase := "0x8888f1f195afa192cfee860698584c030f4c9db1"
	g := startGethWithRpc(t, "lightserver", "--mine", "--miner.etherbase", etherbase, "--syncmode=fast", "--light.serve=100", "--light.maxpeers=1", "--nodiscover", "--nat=extip:127.0.0.1")
	return g
}
func startClient(t *testing.T, name string) *gethrpc {
	runGeth(t, "init", "./testdata/genesis.json").WaitExit()
	g := startGethWithRpc(t, name, "--nodiscover", "--syncmode=light")
	return g
}

func TestPriorityClient(t *testing.T) {
	// Init and start server
	server := startServer(t)
	defer server.killAndWait()

	lightServer := startLightServer(t)
	defer lightServer.killAndWait()

	// Make the lightServer sync to the server
	// This is the only way to make the lightServer synced
	lightServer.addPeer(server, 1)
	lightServer.waitSynced()

	// Start client and add lightServer as peer
	client := startClient(t, "client")
	defer client.killAndWait()
	client.addPeer(lightServer, 1)
	var peers []*p2p.PeerInfo
	client.callRPC(&peers, "admin_peers")
	if len(peers) != 1 {
		t.Errorf("Expected: # of client peers == 1, actual: %v", len(peers))
		return
	}

	// Set up priority client, get its nodeID, increase its balance on the lightServer
	prio := startClient(t, "prio")
	defer prio.killAndWait()
	// 3_000_000_000 once we move to Go 1.13
	tokens := 3000000000
	lightServer.callRPC(nil, "les_addBalance", prio.getNodeInfo().ID, tokens, "foobar")
	// We expect two events, adding prio and removing the old client
	prio.addPeer(lightServer, 1)

	// Check if priority client is actually syncing and the regular client got kicked out
	prio.callRPC(&peers, "admin_peers")
	if len(peers) != 1 {
		t.Errorf("Expected: # of prio peers == 1, actual: %v", len(peers))
	}

	lightServer.callRPC(&peers, "admin_peers")
	for _, p := range peers {
		if p.Enode == client.getNodeInfo().Enode {
			t.Error("client is still a peer of lightServer")
		}
	}

	client.callRPC(&peers, "admin_peers")
	if len(peers) != 0 {
		t.Errorf("Expected: # of client peers == 0, actual: %v", len(peers))
	}
}
