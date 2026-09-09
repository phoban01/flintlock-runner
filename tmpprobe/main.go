package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

func signer() ssh.Signer {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		panic(err)
	}
	return s
}

func main() {
	hostKey := signer()
	clientKey := signer()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostKey)

	client, server := net.Pipe()
	go func() {
		c, chans, reqs, err := ssh.NewServerConn(server, cfg)
		fmt.Println("server handshake:", err)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for ch := range chans {
			_ = ch.Reject(ssh.Prohibited, "no")
		}
		_ = c.Close()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, _, _, err := ssh.NewClientConn(client, "microvm", &ssh.ClientConfig{
			User:            "runner",
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		})
		fmt.Println("client handshake:", err)
		if c != nil {
			_ = c.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		fmt.Println("TIMEOUT")
	}
}
