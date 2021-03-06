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

// Package link contains the encrypted network link implementation.
package link

import (
	"bytes"
	"crypto/cipher"
	"crypto/hmac"
	"encoding/gob"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net"
	"time"

	"github.com/karalabe/iris/config"
	"github.com/karalabe/iris/proto"
	"github.com/karalabe/iris/proto/stream"
)

// Link termination message for graceful tear-down.
type closePacket struct {
}

// Make sure the close packet is registered with gob.
func init() {
	gob.Register(&closePacket{})
}

// Accomplishes secure and authenticated full duplex communication. Note, only
// the headers are encrypted and decrypted. It is the responsibility of the
// caller to call proto.Message.Encrypt/Decrypt (link would bottleneck).
type Link struct {
	socket *stream.Stream

	inCipher  cipher.Stream
	outCipher cipher.Stream

	inMacer  hash.Hash
	outMacer hash.Hash

	inBuffer  bytes.Buffer
	outBuffer bytes.Buffer

	inCoder  *gob.Decoder
	outCoder *gob.Encoder

	inHeadBuf []byte
	inMacBuf  []byte

	Send     chan *proto.Message
	Recv     chan *proto.Message
	sendQuit chan chan error
	recvQuit chan chan error
}

// Creates a new, full-duplex encrypted link from the negotiated secret. The
// client is used to decide the key derivation order for the two half-duplex
// channels (server keys first, client key second).
func New(conn *stream.Stream, hkdf io.Reader, server bool) *Link {
	l := &Link{
		socket: conn,
	}
	// Create the duplex channel
	sc, sm := makeHalfDuplex(hkdf)
	cc, cm := makeHalfDuplex(hkdf)
	if server {
		l.inCipher, l.outCipher, l.inMacer, l.outMacer = cc, sc, cm, sm
	} else {
		l.inCipher, l.outCipher, l.inMacer, l.outMacer = sc, cc, sm, cm
	}
	// Create the gob coders
	l.inCoder = gob.NewDecoder(&l.inBuffer)
	l.outCoder = gob.NewEncoder(&l.outBuffer)

	return l
}

// Assembles the crypto primitives needed for a one way communication channel:
// the stream cipher for encryption and the mac for authentication.
func makeHalfDuplex(hkdf io.Reader) (cipher.Stream, hash.Hash) {
	// Extract the symmetric key and create the block cipher
	key := make([]byte, config.SessionCipherBits/8)
	n, err := io.ReadFull(hkdf, key)
	if n != len(key) || err != nil {
		panic(fmt.Sprintf("Failed to extract session key: %v", err))
	}
	block, err := config.SessionCipher(key)
	if err != nil {
		panic(fmt.Sprintf("Failed to create session cipher: %v", err))
	}
	// Extract the IV for the counter mode and create the stream cipher
	iv := make([]byte, block.BlockSize())
	n, err = io.ReadFull(hkdf, iv)
	if n != len(iv) || err != nil {
		panic(fmt.Sprintf("Failed to extract session IV: %v", err))
	}
	stream := cipher.NewCTR(block, iv)

	// Extract the HMAC key and create the session MACer
	salt := make([]byte, config.SessionHash().Size())
	n, err = io.ReadFull(hkdf, salt)
	if n != len(salt) || err != nil {
		panic(fmt.Sprintf("Failed to extract session mac salt: %v", err))
	}
	mac := hmac.New(config.SessionHash, salt)

	return stream, mac
}

// Creates the buffer channels and starts the transfer processes.
func (l *Link) Start(cap int) {
	// Create the data and quit channels
	l.Send = make(chan *proto.Message, cap)
	l.Recv = make(chan *proto.Message, cap)
	l.sendQuit = make(chan chan error)
	l.recvQuit = make(chan chan error)

	// Start the transfers
	go l.sender()
	go l.receiver()
}

// Terminates any live data transfer go routines and closes the underlying sock.
func (l *Link) Close() error {
	var res error

	// Set a maximum timeout for the graceful closes to finish
	l.socket.Sock().SetDeadline(time.Now().Add(config.SessionGraceTimeout))

	// Terminate the sender, giving it a chance to deliver queued messages
	if l.sendQuit != nil {
		errc := make(chan error)
		l.sendQuit <- errc
		if err := <-errc; res == nil {
			res = err
		}
	}
	// Terminate the receiver, giving it a chance to deliver until remotely closed
	if l.recvQuit != nil {
		errc := make(chan error)
		l.recvQuit <- errc
		if err := <-errc; res == nil {
			res = err
		}
	}
	// Terminate the network stream socket
	if err := l.socket.Close(); res == nil {
		res = err
	}
	return res
}

// The actual message sending logic. Calculates the payload MAC, encrypts the
// headers and sends it down to the stream. Direct send is public for handshake
// simplifications. After that is done, the link should switch to channel mode.
func (l *Link) SendDirect(msg *proto.Message) error {
	var err error

	// Sanity check for message data security
	if !msg.Secure() && len(msg.Data) > 0 {
		log.Printf("link: unsecured data, send denied.")
		return errors.New("unsecured data, send denied")
	}
	// Flatten and encrypt the headers
	if err = l.outCoder.Encode(msg.Head); err != nil {
		return err
	}
	l.outCipher.XORKeyStream(l.outBuffer.Bytes(), l.outBuffer.Bytes())
	defer l.outBuffer.Reset()

	// Generate the MAC of the encrypted payload and headers
	l.outMacer.Write(l.outBuffer.Bytes())
	l.outMacer.Write(msg.Data)

	// Send the multi-part message (headers + payload + MAC)
	if err = l.socket.Send(l.outBuffer.Bytes()); err != nil {
		return err
	}
	if err = l.socket.Send(msg.Data); err != nil {
		return err
	}
	if err = l.socket.Send(l.outMacer.Sum(nil)); err != nil {
		return err
	}
	return l.socket.Flush()
}

// The actual message receiving logic. Reads a message from the stream, verifies
// its mac, decodes the headers and send it upwards. Direct receive is public for
// handshake simplifications, after which the link should switch to channel mode.
func (l *Link) RecvDirect() (*proto.Message, error) {
	var msg proto.Message
	var err error

	// Retrieve a new package
	if err = l.socket.Recv(&l.inHeadBuf); err != nil {
		return nil, err
	}
	if err = l.socket.Recv(&msg.Data); err != nil {
		return nil, err
	}
	if err = l.socket.Recv(&l.inMacBuf); err != nil {
		return nil, err
	}
	// Verify the message contents (payload + header)
	l.inMacer.Write(l.inHeadBuf)
	l.inMacer.Write(msg.Data)
	if !bytes.Equal(l.inMacBuf, l.inMacer.Sum(nil)) {
		err = errors.New(fmt.Sprintf("mac mismatch: have %v, want %v.", l.inMacer.Sum(nil), l.inMacBuf))
		return nil, err
	}
	// Extract the package contents
	l.inCipher.XORKeyStream(l.inHeadBuf, l.inHeadBuf)
	l.inBuffer.Write(l.inHeadBuf)
	if err = l.inCoder.Decode(&msg.Head); err != nil {
		return nil, err
	}
	// Set the message security knowingly to true
	msg.KnownSecure()
	return &msg, nil
}

// Sends messages from the upper layers into the encrypted link.
func (l *Link) sender() {
	var errc chan error
	var errv error

	// Loop until an error occurs or quit is requested
	for errv == nil && errc == nil {
		select {
		case errc = <-l.sendQuit:
			continue
		case msg := <-l.Send:
			errv = l.SendDirect(msg)
		}
	}
	// If quit was requested, send all pending messages and close packet
	if errc != nil {
		// Flush all pending messages
		for done := false; !done && errv == nil; {
			select {
			case msg := <-l.Send:
				errv = l.SendDirect(msg)
			default:
				done = true
			}
		}
		// Send the final close packet
		if errv == nil {
			errv = l.SendDirect(&proto.Message{
				Head: proto.Header{
					Meta: &closePacket{},
				},
			})
		}
	} else {
		// Error, wait for channel to report on
		errc = <-l.sendQuit
	}
	errc <- errv
}

// Transfers messages from the session to the upper layers decoding the headers.
func (l *Link) receiver() {
	var errc chan error
	var errv error

	// Loop until an error occurs or quit is requested
	for errv == nil && errc == nil {
		// Fetch the next message from the encrypted link
		msg, err := l.RecvDirect()
		if err != nil {
			errv = err
			continue
		}
		// Check if it's a remote close packet
		if _, ok := msg.Head.Meta.(*closePacket); ok {
			break
		}
		// Transfer upwards, or terminate
		select {
		case l.Recv <- msg:
			// Ok, upstream handled
		default:
			// Only check for termination if upstream blocked (i.e. flush pending messages first)
			select {
			case l.Recv <- msg:
				// Ok, upstream unblocked
			case errc = <-l.recvQuit:
				// Terminating
			}
		}
	}
	// Close the upward stream and sync termination
	close(l.Recv)
	if errc == nil {
		errc = <-l.recvQuit
	}
	errc <- errv
}

// Retrieves the raw connection object if special manipulations are needed.
func (l *Link) Sock() *net.TCPConn {
	return l.socket.Sock()
}
