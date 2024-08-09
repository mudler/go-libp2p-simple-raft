package simpleraft

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/ipfs/go-log/v2"
	consensus "github.com/libp2p/go-libp2p-consensus"
	raft "github.com/libp2p/go-libp2p-raft"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

var raftLogger = log.Logger("raft-cluster")

var waitForUpdatesInterval = 400 * time.Millisecond

type SimpleRaft struct {
	sync.Mutex
	host     host.Host
	raft     *hraft.Raft
	actor    *raft.Actor
	nethosts []string

	peerStores map[string]peer.AddrInfo

	transport     *hraft.NetworkTransport
	snapshotStore hraft.SnapshotStore
	consensus     *raft.Consensus
	logStore      hraft.LogStore
}

func (rw *SimpleRaft) GetPeers() []peer.AddrInfo {
	rw.Lock()
	defer rw.Unlock()
	peers := make([]peer.AddrInfo, 0)
	for _, h := range rw.nethosts {
		peers = append(peers, rw.peerStores[h])
	}
	return peers
}

func (rw *SimpleRaft) GetLeader() (peer.AddrInfo, error) {
	rw.Lock()
	defer rw.Unlock()
	leader, err := rw.actor.Leader()
	if err != nil {
		return peer.AddrInfo{}, err
	}

	return rw.peerStores[leader.String()], nil
}

func NewSimpleRaft(h host.Host, state consensus.State, peerChan chan peer.AddrInfo) (*SimpleRaft, error) {

	servers := make([]hraft.Server, 0)
	for _, h := range []host.Host{h} {
		servers = append(servers, hraft.Server{
			Suffrage: hraft.Voter,
			ID:       hraft.ServerID(h.ID().String()),
			Address:  hraft.ServerAddress(h.ID().String()),
		})
	}
	serversCfg := hraft.Configuration{Servers: servers}

	// Create Raft Configs. The Local ID is the PeerOID
	config1 := hraft.DefaultConfig()
	config1.LogOutput = os.Stdout
	config1.Logger = nil
	config1.LocalID = hraft.ServerID(h.ID().String())
	snapshots1 := hraft.NewInmemSnapshotStore()
	logStore1 := hraft.NewInmemStore()

	transport, err := raft.NewLibp2pTransport(h, time.Second*10)
	if err != nil {
		return nil, err
	}

	err = hraft.BootstrapCluster(config1, logStore1, logStore1, snapshots1, transport, serversCfg.Clone())
	if err != nil {
		return nil, err
	}
	consensus := raft.NewConsensus(state)

	r, err := hraft.NewRaft(config1, consensus.FSM(), logStore1, logStore1, snapshots1, transport)
	if err != nil {
		return nil, err
	}

	actor1 := raft.NewActor(r)
	consensus.SetActor(actor1)

	w := &SimpleRaft{
		consensus:     consensus,
		host:          h,
		transport:     transport,
		actor:         actor1,
		snapshotStore: snapshots1,
		logStore:      logStore1,
		raft:          r,
		peerStores:    make(map[string]peer.AddrInfo),
	}
	// If the peer disconnects we will also remove it from the raft cluster
	h.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(net network.Network, conn network.Conn) {
			w.Lock()
			defer w.Unlock()
			//logger.Info("peer disconnected: ", conn.RemotePeer().String())
			//logger.Info("removing peer from raft: ", conn.RemotePeer().String())
			future := r.RemoveServer(hraft.ServerID(conn.RemotePeer().String()), 0, 0)
			err := future.Error()
			if err != nil {
				fmt.Println("error", err)
				//logger.Error("raft cannot remove peer: ", err)
			}

			// remove it from nethosts
			for i, h := range w.nethosts {
				if h == conn.RemotePeer().String() {
					w.nethosts = append(w.nethosts[:i], w.nethosts[i+1:]...)
					delete(w.peerStores, h)
				}
			}
		},
	})

	w.trackPeers(context.Background(), peerChan)
	return w, nil
}

func (rw *SimpleRaft) IsLeader() bool {
	return rw.actor.IsLeader()
}

func (rw *SimpleRaft) VerifyLeader() error {
	future := rw.raft.VerifyLeader()
	return future.Error()
}

func (rw *SimpleRaft) GetLeaderID() (leaderAddress hraft.ServerAddress, serverID hraft.ServerID) {
	return rw.raft.LeaderWithID()
}

func (rw *SimpleRaft) CommitState(state consensus.State) (consensus.State, error) {
	return rw.consensus.CommitState(state)
}

func (rw *SimpleRaft) GetState() (consensus.State, error) {
	return rw.consensus.GetCurrentState()
}

// WaitForSync waits for a leader and for the state to be up to date, then returns.
func (rw *SimpleRaft) WaitForSync(ctx context.Context) error {

	leaderCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// 1 - wait for leader
	// 2 - wait until we are a Voter
	// 3 - wait until last index is applied

	// From raft docs:

	// once a staging server receives enough log entries to be sufficiently
	// caught up to the leader's log, the leader will invoke a  membership
	// change to change the Staging server to a Voter

	// Thus, waiting to be a Voter is a guarantee that we have a reasonable
	// up to date state. Otherwise, we might return too early (see
	// https://github.com/ipfs-cluster/ipfs-cluster/issues/378)

	_, err := rw.WaitForLeader(leaderCtx)
	if err != nil {
		return errors.New("error waiting for leader: " + err.Error())
	}

	err = rw.WaitForVoter(ctx)
	if err != nil {
		return errors.New("error waiting to become a Voter: " + err.Error())
	}

	err = rw.WaitForUpdates(ctx)
	if err != nil {
		return errors.New("error waiting for consensus updates: " + err.Error())
	}
	return nil
}

// WaitForUpdates holds until Raft has synced to the last index in the log
func (rw *SimpleRaft) WaitForUpdates(ctx context.Context) error {

	raftLogger.Debug("Raft state is catching up to the latest known version. Please wait...")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			lai := rw.raft.AppliedIndex()
			li := rw.raft.LastIndex()
			raftLogger.Debugf("current Raft index: %d/%d",
				lai, li)
			if lai == li {
				return nil
			}
			time.Sleep(waitForUpdatesInterval)
		}
	}
}

// WaitForLeader holds until Raft says we have a leader.
// Returns if ctx is canceled.
func (rw *SimpleRaft) WaitForLeader(ctx context.Context) (string, error) {
	ticker := time.NewTicker(time.Second / 2)
	for {
		select {
		case <-ticker.C:
			if l := rw.raft.Leader(); l != "" {
				raftLogger.Debug("waitForleaderTimer")
				raftLogger.Infof("Current Raft Leader: %s", l)
				ticker.Stop()
				return string(l), nil
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (rw *SimpleRaft) trackPeers(ctx context.Context, peerChan chan peer.AddrInfo) {
	// create a chan for the peers that we failed to add as a voter
	// so we can retry later
	failedPeers := make(chan peer.AddrInfo, 10)

	tryPeer := func(p peer.AddrInfo) {
		if !rw.IsLeader() {
			// TODO: change this and store it internally.
			// We need to retry if we are
			fmt.Println("not a leader")
			//failedPeers <- p
			return
		}
		fmt.Println("Adding peer", p.ID.String(), p.Addrs)

		if err := rw.host.Connect(ctx, p); err != nil {
			fmt.Println("Connection failed:", err)
		}
		//rw.host.ConnManager().TagPeer(p.ID, "raft", 10)

		future := rw.raft.AddVoter(
			hraft.ServerID(p.ID.String()),
			hraft.ServerAddress(p.ID.String()),
			0,
			0,
		) // TODO: Extra cfg value?
		err := future.Error()
		if err != nil {
			fmt.Println("error", err)
			failedPeers <- p
			//logger.Error("raft cannot add peer: ", err)
		}

		// add it to nethosts if doesn't exist already
		for _, h := range rw.nethosts {
			if h == p.ID.String() {
				return
			}
		}
		rw.Lock()
		defer rw.Unlock()
		rw.nethosts = append(rw.nethosts, p.ID.String())
		rw.peerStores[p.ID.String()] = p
	}
	go func() {
		for {
			select {

			case <-ctx.Done():
				return
			case p := <-peerChan:
				tryPeer(p)
			case p := <-failedPeers:
				tryPeer(p)
			}
		}
	}()
}

func (rw *SimpleRaft) WaitForVoter(ctx context.Context) error {
	raftLogger.Debug("waiting until we are promoted to a voter")

	pid := hraft.ServerID(rw.host.ID().String())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			raftLogger.Debugf("%s: get configuration", pid)
			configFuture := rw.raft.GetConfiguration()
			if err := configFuture.Error(); err != nil {
				return err
			}

			if isVoter(pid, configFuture.Configuration()) {
				return nil
			}
			raftLogger.Debugf("%s: not voter yet", pid)

			time.Sleep(waitForUpdatesInterval)
		}
	}
}

func isVoter(srvID hraft.ServerID, cfg hraft.Configuration) bool {
	for _, server := range cfg.Servers {
		if server.ID == srvID && server.Suffrage == hraft.Voter {
			return true
		}
	}
	return false
}
