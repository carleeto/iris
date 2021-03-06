// Iris - Decentralized Messaging Framework
// Copyright 2014 Peter Szilagyi. All rights reserved.
//
// Iris is dual licensed: you can redistribute it and/or modify it under the
// terms of the GNU General Public License as published by the Free Software
// Foundation, either version 3 of the License, or (at your option) any later
// version.
//
// The framework is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE.  See the GNU General Public License for
// more details.
//
// Alternatively, the Iris framework may be used in accordance with the terms
// and conditions contained in a signed written agreement between you and the
// author(s).
//
// Author: peterke@gmail.com (Peter Szilagyi)

package iris

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karalabe/iris/config"
)

// Connection handler for the req/rep tests.
type requester struct {
	self   int    // Index of the owner node
	remote uint32 // Number of remote requests
}

func (r *requester) HandleBroadcast(msg []byte) {
	panic("Broadcast passed to request handler")
}

func (r *requester) HandleRequest(req []byte, timeout time.Duration) []byte {
	if r.self != int(req[0]) {
		atomic.AddUint32(&r.remote, 1)
	}
	return req
}

func (r *requester) HandleTunnel(tun *Tunnel) {
	panic("Inbound tunnel on request handler")
}

func (r *requester) HandleDrop(reason error) {
	panic("Connection dropped on request handler")
}

// Individual reqrep tests.
func TestReqRepSingleNodeSingleConn(t *testing.T) {
	testReqRep(t, 1, 1, 10000)
}

func TestReqRepSingleNodeMultiConn(t *testing.T) {
	testReqRep(t, 1, 10, 1000)
}

func TestReqRepMultiNodeSingleConn(t *testing.T) {
	testReqRep(t, 10, 1, 1000)
}

func TestReqRepMultiNodeMultiConn(t *testing.T) {
	testReqRep(t, 10, 10, 100)
}

// Tests multi node multi connection request/replies.
func testReqRep(t *testing.T, nodes, conns, reqs int) {
	// Configure the test
	swapConfigs()
	defer swapConfigs()

	olds := config.BootPorts
	for i := 0; i < nodes; i++ {
		config.BootPorts = append(config.BootPorts, 65000+i)
	}
	defer func() { config.BootPorts = olds }()

	key, _ := x509.ParsePKCS1PrivateKey(privKeyDer)
	overlay := "reqrep-test"
	cluster := fmt.Sprintf("reqrep-test-%d-%d", nodes, conns)

	// Boot the iris overlays
	liveNodes := make([]*Overlay, nodes)
	for i := 0; i < nodes; i++ {
		liveNodes[i] = New(overlay, key)
		if _, err := liveNodes[i].Boot(); err != nil {
			t.Fatalf("failed to boot iris overlay: %v.", err)
		}
		defer func(node *Overlay) {
			if err := node.Shutdown(); err != nil {
				t.Fatalf("failed to terminate iris node: %v.", err)
			}
		}(liveNodes[i])
	}
	// Connect to all nodes with a lot of clients
	liveHands := make(map[int][]*requester)
	liveConns := make(map[int][]*Connection)
	for i, node := range liveNodes {
		liveHands[i] = make([]*requester, conns)
		liveConns[i] = make([]*Connection, conns)
		for j := 0; j < conns; j++ {
			liveHands[i][j] = &requester{i, 0}
			conn, err := node.Connect(cluster, liveHands[i][j])
			if err != nil {
				t.Fatalf("failed to connect to the iris overlay: %v.", err)
			}
			liveConns[i][j] = conn

			defer func(conn *Connection) {
				if err := conn.Close(); err != nil {
					t.Fatalf("failed to close iris connection: %v.", err)
				}
			}(liveConns[i][j])
		}
	}
	// Make sure there is a little time to propagate state and reports (TODO, fix this)
	if nodes > 1 {
		time.Sleep(3 * time.Second)
	}
	// Request with each and every node in parallel
	pend := new(sync.WaitGroup)
	for i := 0; i < nodes; i++ {
		for j := 0; j < conns; j++ {
			for k := 0; k < reqs; k++ {
				pend.Add(1)
				go func(i, j, k int) {
					defer pend.Done()
					orig := []byte{byte(i), byte(j), byte(k)}
					req := make([]byte, len(orig))
					copy(req, orig)

					if rep, err := liveConns[i][j].Request(cluster, req, 5*time.Second); err != nil {
						t.Fatalf("failed to send request: %v.", err)
					} else if bytes.Compare(orig, rep) != 0 {
						t.Fatalf("req/rep mismatch: have %v, want %v.", rep, orig)
					}
				}(i, j, k)
			}
		}
	}
	pend.Wait()

	// Log some warning if connections didn't get remote requests
	if nodes > 1 {
		for i := 0; i < nodes; i++ {
			for j := 0; j < conns; j++ {
				if liveHands[i][j].remote == 0 {
					t.Logf("%v:%v no remote requests received.", i, j)
				}
			}
		}
	}
}
