// Iris - Decentralized Messaging Framework
// Copyright 2013 Peter Szilagyi. All rights reserved.
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

package overlay

import (
	"crypto/x509"
	"math/big"
	"sort"
	"testing"
	"time"

	"github.com/karalabe/iris/config"
	"github.com/karalabe/iris/ext/mathext"
)

func checkRoutes(t *testing.T, nodes []*Overlay) {
	// Extract the ids from the running nodes
	ids := make([]*big.Int, len(nodes))
	for i, o := range nodes {
		ids[i] = o.nodeId
	}
	// Assemble the leafset of each node and verify
	for _, o := range nodes {
		sort.Sort(idSlice{o.nodeId, ids})
		origin := 0
		for o.nodeId.Cmp(ids[origin]) != 0 {
			origin++
		}
		min := mathext.MaxInt(0, origin-config.OverlayLeaves/2)
		max := mathext.MinInt(len(ids), origin+config.OverlayLeaves/2)
		leaves := ids[min:max]

		if len(leaves) != len(o.routes.leaves) {
			t.Fatalf("overlay %v: leafset mismatch: have %v, want %v.", o.nodeId, o.routes.leaves, leaves)
		} else {
			for i, leaf := range leaves {
				if leaf.Cmp(o.routes.leaves[i]) != 0 {
					t.Fatalf("overlay %v: leafset mismatch: have %v, want %v.", o.nodeId, o.routes.leaves, leaves)
					break
				}
			}
		}
	}
	// Check the routing table for each node
	for _, o := range nodes {
		for r, row := range o.routes.routes {
			for c, p := range row {
				if p == nil {
					// Check that indeed no id is valid for this entry
					for _, id := range ids {
						if id.Cmp(o.nodeId) != 0 {
							if pre, dig := prefix(o.nodeId, id); pre == r && dig == c {
								t.Fatalf("overlay %v: entry {%v, %v} missing: %v.", o.nodeId, r, c, id)
							}
						}
					}
				} else {
					// Check that the id is valid and indeed not some leftover
					if pre, dig := prefix(o.nodeId, p); pre != r || dig != c {
						t.Fatalf("overlay %v: entry {%v, %v} invalid: %v.", o.nodeId, r, c, p)
					}
					alive := false
					for _, id := range ids {
						if id.Cmp(p) == 0 {
							alive = true
							break
						}
					}
					if !alive {
						t.Fatalf("overlay %v: entry {%v, %v} already dead: %v.", o.nodeId, r, c, p)
					}
				}
			}
		}
	}
}

func TestMaintenance(t *testing.T) {
	// Override the boot and convergence times
	boot, conv := 250*time.Millisecond, 50*time.Millisecond

	config.OverlayBootTimeout, boot = boot, config.OverlayBootTimeout
	config.OverlayConvTimeout, conv = conv, config.OverlayConvTimeout

	defer func() {
		config.OverlayBootTimeout, boot = boot, config.OverlayBootTimeout
		config.OverlayConvTimeout, conv = conv, config.OverlayConvTimeout
	}()

	originals := 3
	additions := 2

	// Make sure there are enough ports to use
	olds := config.BootPorts
	defer func() { config.BootPorts = olds }()

	for i := 0; i < originals+additions; i++ {
		config.BootPorts = append(config.BootPorts, 65520+i)
	}
	// Parse encryption key
	key, _ := x509.ParsePKCS1PrivateKey(privKeyDer)

	// Start handful of nodes and ensure valid routing state
	nodes := []*Overlay{}
	for i := 0; i < originals; i++ {
		nodes = append(nodes, New(appId, key, new(nopCallback)))
		if _, err := nodes[i].Boot(); err != nil {
			t.Fatalf("failed to boot nodes: %v.", err)
		}
		defer nodes[i].Shutdown()
	}
	// Check the routing tables
	checkRoutes(t, nodes)

	// Start some additional nodes and ensure still valid routing state
	for i := 0; i < additions; i++ {
		nodes = append(nodes, New(appId, key, new(nopCallback)))
		if _, err := nodes[len(nodes)-1].Boot(); err != nil {
			t.Fatalf("failed to boot nodes: %v.", err)
		}
	}
	// Check the routing tables
	checkRoutes(t, nodes)

	// Terminate some nodes, and ensure still valid routing state
	for i := 0; i < additions; i++ {
		nodes[originals+i].Shutdown()
	}
	nodes = nodes[:originals]

	// Wait a while for state updates to propagate
	time.Sleep(time.Second)

	// Check the routing tables
	checkRoutes(t, nodes)
}

/*
func TestMaintenanceDOS(t *testing.T) {
	// Make sure there are enough ports to use (use a huge number to simplify test code)
	olds := config.BootPorts
	defer func() { config.BootPorts = olds }()
	for i := 0; i < 16; i++ {
		config.BootPorts = append(config.BootPorts, 40000+i)
	}
	// Parse encryption key
	key, _ := x509.ParsePKCS1PrivateKey(privKeyDer)

	// Increment the overlays till the test fails
	for peers := 4; !t.Failed(); peers++ {
		log.Printf("running maintenance for %d peers.", peers)

		// Start the batch of nodes
		nodes := []*Overlay{}
		for i := 0; i < peers; i++ {
			nodes = append(nodes, New(appId, key, nil))
			go func(o *Overlay) {
				if _, err := o.Boot(); err != nil {
					t.Fatalf("failed to boot nodes: %v.", err)
				}
			}(nodes[i])
		}
		// Wait a while for the handshakes to complete
		//		time.Sleep(10 * time.Second)
		done := time.After(10 * time.Second)
		for loop := true; loop; {
			select {
			case <-done:
				loop = false
			case <-time.After(250 * time.Millisecond):
				log.Printf("Live go routines: %d.", runtime.NumGoroutine())
			}
		}
		// Check the routing tables
		checkRoutes(t, nodes)

		// Terminate all nodes, irrelevent of their state
		for i := 0; i < peers; i++ {
			nodes[i].Shutdown()
		}
	}
}
*/
