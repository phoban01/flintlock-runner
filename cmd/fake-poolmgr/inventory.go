package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// inventoryFile is the YAML the -inventory flag names. It is a subset of the
// Fleet Controller's Inventory file (FL-060, CF-030) with one addition,
// token_file, so that a token can be kept out of the document; every other
// field of a Host entry, such as its architecture and capacity, is ignored
// here because a pool manager only needs to reach the Host.
type inventoryFile struct {
	Hosts []inventoryHost `yaml:"hosts"`
}

// inventoryHost is one flintlockd endpoint.
type inventoryHost struct {
	// Name is the name Pools use in flintlock_hosts (CF-032).
	Name string `yaml:"name"`
	// Endpoint is the flintlockd gRPC address, host:port. Address is
	// accepted as an alias so that a hand-written file can use either.
	Endpoint string `yaml:"endpoint"`
	Address  string `yaml:"address"`
	// Token is the flintlockd basic auth token (HO-004); TokenFile is a file
	// holding it, whose trailing newline is trimmed.
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
	// TLS is the client TLS material for this Host (HO-005, HO-006).
	TLS inventoryTLS `yaml:"tls"`
}

// inventoryTLS mirrors config.ClientTLS.
type inventoryTLS struct {
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// Insecure dials plaintext and has to be explicit (SE-021).
	Insecure bool `yaml:"insecure"`
}

// loadInventory reads the inventory file at path and converts it to the
// endpoints the fake Pool Manager dials. Unknown fields are ignored, so the
// Inventory the Fleet Controller writes can be passed as it is.
func loadInventory(path string) ([]flintlock.Endpoint, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	var file inventoryFile
	if err := yaml.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("inventory %s: %w", path, err)
	}
	if len(file.Hosts) == 0 {
		return nil, fmt.Errorf("inventory %s: no hosts", path)
	}
	endpoints := make([]flintlock.Endpoint, 0, len(file.Hosts))
	for i, h := range file.Hosts {
		ep, err := h.endpoint()
		if err != nil {
			return nil, fmt.Errorf("inventory %s: hosts[%d]: %w", path, i, err)
		}
		endpoints = append(endpoints, ep)
	}
	return endpoints, nil
}

// endpoint converts one entry, reading the token file when there is one.
func (h inventoryHost) endpoint() (flintlock.Endpoint, error) {
	address := h.Endpoint
	if address == "" {
		address = h.Address
	}
	if h.Name == "" {
		return flintlock.Endpoint{}, fmt.Errorf("name is required")
	}
	if address == "" {
		return flintlock.Endpoint{}, fmt.Errorf("host %q: endpoint is required", h.Name)
	}
	if h.Token != "" && h.TokenFile != "" {
		return flintlock.Endpoint{}, fmt.Errorf("host %q: token and token_file are mutually exclusive", h.Name)
	}
	if h.TLS.Insecure && (h.TLS.CAFile != "" || h.TLS.CertFile != "" || h.TLS.KeyFile != "") {
		return flintlock.Endpoint{}, fmt.Errorf("host %q: tls.insecure with tls files is contradictory", h.Name)
	}
	if (h.TLS.CertFile == "") != (h.TLS.KeyFile == "") {
		return flintlock.Endpoint{}, fmt.Errorf("host %q: tls.cert_file and tls.key_file go together", h.Name)
	}
	token := h.Token
	if h.TokenFile != "" {
		body, err := os.ReadFile(h.TokenFile)
		if err != nil {
			return flintlock.Endpoint{}, fmt.Errorf("host %q: token_file: %w", h.Name, err)
		}
		token = strings.TrimSpace(string(body))
		if token == "" {
			return flintlock.Endpoint{}, fmt.Errorf("host %q: token_file %s is empty", h.Name, h.TokenFile)
		}
	}
	return flintlock.Endpoint{
		Name:    h.Name,
		Address: address,
		Token:   token,
		TLS: flintlock.TLSOptions{
			CAFile:   h.TLS.CAFile,
			CertFile: h.TLS.CertFile,
			KeyFile:  h.TLS.KeyFile,
			Insecure: h.TLS.Insecure,
		},
	}, nil
}
