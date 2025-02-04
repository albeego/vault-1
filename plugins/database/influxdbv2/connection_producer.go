package influxdbv2

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/go-secure-stdlib/parseutil"
	"github.com/hashicorp/go-secure-stdlib/tlsutil"
	"github.com/hashicorp/vault/sdk/database/dbplugin/v5"
	"github.com/hashicorp/vault/sdk/database/helper/connutil"
	"github.com/hashicorp/vault/sdk/helper/certutil"
	"github.com/influxdata/influxdb-client-go/v2"
	"github.com/mitchellh/mapstructure"
)

// influxdbConnectionProducer implements ConnectionProducer and provides an
// interface for influxdb databases to make connections.
type influxdbConnectionProducer struct {
	Host              string      `json:"host" structs:"host" mapstructure:"host"`
	Token             string      `json:"token" structs:"token" mapstructure:"token"`
	Port              string      `json:"port" structs:"port" mapstructure:"port"` // default to 8086
	TLS               bool        `json:"tls" structs:"tls" mapstructure:"tls"`
	InsecureTLS       bool        `json:"insecure_tls" structs:"insecure_tls" mapstructure:"insecure_tls"`
	ConnectTimeoutRaw interface{} `json:"connect_timeout" structs:"connect_timeout" mapstructure:"connect_timeout"`
	TLSMinVersion     string      `json:"tls_min_version" structs:"tls_min_version" mapstructure:"tls_min_version"`
	PemBundle         string      `json:"pem_bundle" structs:"pem_bundle" mapstructure:"pem_bundle"`
	PemJSON           string      `json:"pem_json" structs:"pem_json" mapstructure:"pem_json"`
	DefaultBucket     string      `json:"default_bucket" structs:"default_bucket" mapstructure:"default_bucket"`
	Organization      string      `json:"organization" structs:"organization" mapstructure:"organization"`

	connectTimeout time.Duration
	certificate    string
	privateKey     string
	issuingCA      string
	rawConfig      map[string]interface{}

	Initialized bool
	Type        string
	client      influxdb2.Client
	sync.Mutex
}

func (i *influxdbConnectionProducer) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	i.Lock()
	defer i.Unlock()

	i.rawConfig = req.Config

	err := mapstructure.WeakDecode(req.Config, i)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	if i.ConnectTimeoutRaw == nil {
		i.ConnectTimeoutRaw = "5s"
	}
	if i.Port == "" {
		i.Port = "8086"
	}
	i.connectTimeout, err = parseutil.ParseDurationSecond(i.ConnectTimeoutRaw)
	if err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("invalid connect_timeout: %w", err)
	}

	switch {
	case len(i.Host) == 0:
		return dbplugin.InitializeResponse{}, fmt.Errorf("host cannot be empty")
	case len(i.Token) == 0:
		return dbplugin.InitializeResponse{}, fmt.Errorf("token cannot be empty")
	}

	var certBundle *certutil.CertBundle
	var parsedCertBundle *certutil.ParsedCertBundle
	switch {
	case len(i.PemJSON) != 0:
		parsedCertBundle, err = certutil.ParsePKIJSON([]byte(i.PemJSON))
		if err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("could not parse given JSON; it must be in the format of the output of the PKI backend certificate issuing command: %w", err)
		}
		certBundle, err = parsedCertBundle.ToCertBundle()
		if err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("Error marshaling PEM information: %w", err)
		}
		i.certificate = certBundle.Certificate
		i.privateKey = certBundle.PrivateKey
		i.issuingCA = certBundle.IssuingCA
		i.TLS = true

	case len(i.PemBundle) != 0:
		parsedCertBundle, err = certutil.ParsePEMBundle(i.PemBundle)
		if err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("Error parsing the given PEM information: %w", err)
		}
		certBundle, err = parsedCertBundle.ToCertBundle()
		if err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("Error marshaling PEM information: %w", err)
		}
		i.certificate = certBundle.Certificate
		i.privateKey = certBundle.PrivateKey
		i.issuingCA = certBundle.IssuingCA
		i.TLS = true
	}

	// Set initialized to true at this point since all fields are set,
	// and the connection can be established at a later time.
	i.Initialized = true

	if req.VerifyConnection {
		if _, err := i.Connection(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("error verifying connection: %w", err)
		}
	}

	resp := dbplugin.InitializeResponse{
		Config: req.Config,
	}

	return resp, nil
}

func (i *influxdbConnectionProducer) Connection(_ context.Context) (interface{}, error) {
	if !i.Initialized {
		return nil, connutil.ErrNotInitialized
	}

	// If we already have a DB, return it
	if i.client != nil {
		return i.client, nil
	}

	cli, err := i.createClient()
	if err != nil {
		return nil, err
	}

	//  Store the session in backend for reuse
	i.client = cli

	return cli, nil
}

func (i *influxdbConnectionProducer) Close() error {
	// Grab the write lock
	i.Lock()
	defer i.Unlock()

	if i.client != nil {
		i.client.Close()
	}

	i.client = nil

	return nil
}

func (i *influxdbConnectionProducer) createClient() (influxdb2.Client, error) {
	var cli influxdb2.Client
	if i.TLS {
		tlsConfig := &tls.Config{}
		if len(i.certificate) > 0 || len(i.issuingCA) > 0 {
			if len(i.certificate) > 0 && len(i.privateKey) == 0 {
				return nil, fmt.Errorf("found certificate for TLS authentication but no private key")
			}

			certBundle := &certutil.CertBundle{}
			if len(i.certificate) > 0 {
				certBundle.Certificate = i.certificate
				certBundle.PrivateKey = i.privateKey
			}
			if len(i.issuingCA) > 0 {
				certBundle.IssuingCA = i.issuingCA
			}

			parsedCertBundle, err := certBundle.ToParsedCertBundle()
			if err != nil {
				return nil, fmt.Errorf("failed to parse certificate bundle: %w", err)
			}

			tlsConfig, err = parsedCertBundle.GetTLSConfig(certutil.TLSClient)
			if err != nil || tlsConfig == nil {
				return nil, fmt.Errorf("failed to get TLS configuration: tlsConfig:%#v err:%w", tlsConfig, err)
			}
		}

		tlsConfig.InsecureSkipVerify = i.InsecureTLS

		if i.TLSMinVersion != "" {
			var ok bool
			tlsConfig.MinVersion, ok = tlsutil.TLSLookup[i.TLSMinVersion]
			if !ok {
				return nil, fmt.Errorf("invalid 'tls_min_version' in config")
			}
		} else {
			// MinVersion was not being set earlier. Reset it to
			// zero to gracefully handle upgrades.
			tlsConfig.MinVersion = 0
		}

		options := influxdb2.Options{}
		options.SetTLSConfig(tlsConfig)

		cli = influxdb2.NewClientWithOptions(fmt.Sprintf("http://%s:%s", i.Host, i.Port), i.Token, &options)
	} else {
		cli = influxdb2.NewClient(fmt.Sprintf("http://%s:%s", i.Host, i.Port), i.Token)
	}

	// Checking server status
	_, err := cli.Ping(context.Background())
	if err != nil {
		return nil, fmt.Errorf("error checking cluster status: %w", err)
	}

	// verifying infos about the connection
	isSufficientAccess, err := isTokenSufficientAccess(context.Background(), cli, i.Token)
	if err != nil {
		return nil, fmt.Errorf("error getting if provided username is admin: %w", err)
	}
	if !isSufficientAccess {
		return nil, fmt.Errorf("the provided user is missing permissions on the influxDB server")
	}

	return cli, nil
}

func (i *influxdbConnectionProducer) secretValues() map[string]string {
	return map[string]string{
		i.Token:     "[token]",
		i.PemBundle: "[pem_bundle]",
		i.PemJSON:   "[pem_json]",
	}
}

func isTokenSufficientAccess(ctx context.Context, cli influxdb2.Client, token string) (bool, error) {
	authorizations, err := cli.AuthorizationsAPI().GetAuthorizations(ctx)
	if err != nil {
		return false, errors.New("cannot access authorizations API to check token")
	}
	hasUserRead := false
	hasUserWrite := false
	hasOrganizationsRead := false
	hasOrganizationsWrite := false
	for _, authorization := range *authorizations {
		if *authorization.Token == token {
			for _, permission := range *authorization.Permissions {
				if permission.Action == "read" && permission.Resource.Type == "users" {
					hasUserRead = true
				}
				if permission.Action == "write" && permission.Resource.Type == "users" {
					hasUserWrite = true
				}
			}
			for _, permission := range *authorization.Permissions {
				if permission.Action == "read" && permission.Resource.Type == "orgs" {
					hasOrganizationsRead = true
				}
				if permission.Action == "write" && permission.Resource.Type == "orgs" {
					hasOrganizationsWrite = true
				}
			}
		}
	}
	if hasUserRead && hasUserWrite && hasOrganizationsRead && hasOrganizationsWrite {
		return true, nil
	}
	return false, fmt.Errorf("the provided token does not have sufficient permissions in influxdb hasUserRead: %t, hasUserWrite: %t, hasOrganizationsRead: %t, hasOrganizationsWrite: %t", hasUserRead, hasUserWrite, hasOrganizationsRead, hasOrganizationsWrite)
}
