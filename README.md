# go-libp2p-simple-raft

A simple raft wrapper for libp2p.

Motivation: I wanted to experiment and have a simple wrapper to work with raft and libp2p, hence why this.

### Usage

```go
import (
    "github.com/mudler/go-libp2p-simple-raft"
)

// Structure nodes have to agree on
type raftState struct {
    Value int
}

ctx := context.Background()

// start a libp2p node (not done here)

// NewSimpleRaft needs a libp2p host, a state to calculate consensus on, and a channel to listen for new peers that are discovered
w, err := simpleraft.NewSimpleRaft(h, &raftState{Value: 3}, peerChan)
if err != nil {
    fmt.Println(err)
}

// Make sure states are syncronized
err = w.WaitForSync(ctx)
if err != nil {
    fmt.Println(err)
}

// Check if we are leaders and update state
if !w.IsLeader() {
    // we are not leaders, we can only read the state and/or know who is the leader
    state, err:= w.GetState()
    address, serverID := w.GetLeaderID()
    return
}

// verify we are leaders
if err := w.VerifyLeader(); err != nil {
    fmt.Println("error", err)
}

// try to write the state
nUpdates := 0
for {
    if nUpdates >= 1000 {
        break
    }

    newState := &raftState{nUpdates * 2}

    // CommitState() blocks until the state has been
    // agreed upon by everyone
    agreedState, err := w.CommitState(newState)
    if err != nil {
        fmt.Println(err)
        continue
    }
    if agreedState == nil {
        fmt.Println("agreedState is nil: commited on a non-leader?")
        continue
    }
    agreedRaftState := agreedState.(*raftState)
    nUpdates++

    if nUpdates%200 == 0 {
        fmt.Printf("Performed %d updates. Current state value: %d\n",
            nUpdates, agreedRaftState.Value)
    }
}
```