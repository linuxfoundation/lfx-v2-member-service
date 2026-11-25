// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package salesforce provides a Salesforce REST API client for querying and
// mutating Salesforce objects via SOQL and the sObject REST endpoints. It wraps
// the github.com/k-capehart/go-salesforce/v3 library.
package salesforce

import (
	"fmt"
	"log/slog"
	"os"

	sf "github.com/k-capehart/go-salesforce/v3"
)

const (
	// defaultAPIVersion is the default Salesforce REST API version. This must
	// match the package-level apiVersion constant in the go-salesforce library
	// (currently v63.0). A mismatch causes the library's TrimPrefix call in
	// performQuery to fail to strip the version prefix from nextRecordsUrl,
	// producing a doubled path on the second page request and a NOT_FOUND error
	// from the Salesforce REST API.
	defaultAPIVersion = "v63.0"
)

// Config holds the Salesforce connected-app credentials and instance URL
// required to authenticate and execute API calls. Two authentication flows
// are supported:
//
//   - Username/password: set Username, Password, and optionally SecurityToken.
//   - JWT bearer: set Username and ConsumerRSAPem (PEM-encoded private key).
//
// In both cases Domain and ConsumerKey are required. ConsumerSecret is required
// for the username/password flow but not for JWT.
type Config struct {
	// Domain is the Salesforce instance URL (e.g. "https://linuxfoundation.my.salesforce.com").
	Domain string

	// ConsumerKey is the OAuth 2.0 client ID for the connected app.
	ConsumerKey string

	// ConsumerSecret is the OAuth 2.0 client secret (required for username/password and client-credentials flows).
	ConsumerSecret string

	// Username is the Salesforce user for username/password or JWT flows.
	Username string

	// Password is the Salesforce user's password (username/password flow only).
	Password string

	// SecurityToken is appended to the password for the username/password flow. Optional.
	SecurityToken string

	// ConsumerRSAPem is a PEM-encoded RSA private key for the JWT bearer flow.
	ConsumerRSAPem string

	// APIVersion is the Salesforce REST API version (e.g. "v63.0").
	APIVersion string

	// PubSubEndpoint is the Salesforce Pub/Sub API gRPC endpoint
	// (e.g. "api.pubsub.salesforce.com:7443"). Empty disables the CDC consumer
	// entirely (no-op), so existing deployments without the var are unaffected.
	PubSubEndpoint string
}

// ConfigFromEnv builds a Config from environment variables. It returns an error
// if the minimum required variables are not set.
//
// Required in all cases:
//
//	SF_INSTANCE_URL   — Salesforce instance URL.
//	SF_CLIENT_ID      — connected-app consumer key.
//
// Username/password flow (when SF_USERNAME and SF_PASSWORD are set):
//
//	SF_USERNAME       — Salesforce username.
//	SF_PASSWORD       — Salesforce password.
//	SF_CLIENT_SECRET  — connected-app consumer secret.
//	SF_SECURITY_TOKEN — (optional) security token appended to password.
//
// JWT bearer flow (when SF_USERNAME and SF_CONSUMER_RSA_PEM are set):
//
//	SF_USERNAME         — Salesforce username.
//	SF_CONSUMER_RSA_PEM — PEM-encoded RSA private key.
//
// Client-credentials flow (when SF_CLIENT_SECRET is set without SF_USERNAME):
//
//	SF_CLIENT_SECRET — connected-app consumer secret.
//
// Optional:
//
//	SF_API_VERSION                — API version (default: "v63.0").
//	SF_PUBSUB_ENDPOINT            — Salesforce Pub/Sub gRPC endpoint; empty disables the CDC consumer.
func ConfigFromEnv() (Config, error) {
	domain := os.Getenv("SF_INSTANCE_URL")
	if domain == "" {
		return Config{}, fmt.Errorf("SF_INSTANCE_URL environment variable is required")
	}

	consumerKey := os.Getenv("SF_CLIENT_ID")
	if consumerKey == "" {
		return Config{}, fmt.Errorf("SF_CLIENT_ID environment variable is required")
	}

	consumerSecret := os.Getenv("SF_CLIENT_SECRET")
	username := os.Getenv("SF_USERNAME")
	password := os.Getenv("SF_PASSWORD")
	securityToken := os.Getenv("SF_SECURITY_TOKEN")
	consumerRSAPem := os.Getenv("SF_CONSUMER_RSA_PEM")

	apiVersion := os.Getenv("SF_API_VERSION")
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}

	pubSubEndpoint := os.Getenv("SF_PUBSUB_ENDPOINT")

	// Validate that at least one auth flow is satisfiable.
	hasJWT := username != "" && consumerRSAPem != ""
	hasUserPass := username != "" && password != ""
	hasClientCreds := consumerSecret != "" && username == ""

	if !hasJWT && !hasUserPass && !hasClientCreds {
		return Config{}, fmt.Errorf(
			"insufficient Salesforce credentials: set SF_USERNAME + SF_CONSUMER_RSA_PEM (JWT), " +
				"SF_USERNAME + SF_PASSWORD + SF_CLIENT_SECRET (username/password), " +
				"or SF_CLIENT_SECRET alone (client-credentials)",
		)
	}

	return Config{
		Domain:         domain,
		ConsumerKey:    consumerKey,
		ConsumerSecret: consumerSecret,
		Username:       username,
		Password:       password,
		SecurityToken:  securityToken,
		ConsumerRSAPem: consumerRSAPem,
		APIVersion:     apiVersion,
		PubSubEndpoint: pubSubEndpoint,
	}, nil
}

// Init creates and authenticates a go-salesforce client from this Config. The
// authentication flow is selected automatically based on which credentials are
// populated:
//
//  1. JWT bearer — when Username and ConsumerRSAPem are both set.
//  2. Username/password — when Username and Password are both set.
//  3. Client-credentials — when ConsumerSecret is set without Username.
func (c Config) Init() (*sf.Salesforce, error) {
	creds := sf.Creds{
		Domain:         c.Domain,
		ConsumerKey:    c.ConsumerKey,
		ConsumerSecret: c.ConsumerSecret,
		Username:       c.Username,
		Password:       c.Password,
		SecurityToken:  c.SecurityToken,
		ConsumerRSAPem: c.ConsumerRSAPem,
	}

	// Log which flow will be used (without secrets).
	switch {
	case c.Username != "" && c.ConsumerRSAPem != "":
		slog.Info("initializing Salesforce client with JWT bearer flow",
			"domain", c.Domain,
			"username", c.Username,
			"api_version", c.APIVersion,
		)
	case c.Username != "" && c.Password != "":
		slog.Info("initializing Salesforce client with username/password flow",
			"domain", c.Domain,
			"username", c.Username,
			"api_version", c.APIVersion,
		)
	default:
		slog.Info("initializing Salesforce client with client-credentials flow",
			"domain", c.Domain,
			"api_version", c.APIVersion,
		)
	}

	client, err := sf.Init(creds,
		sf.WithAPIVersion(c.APIVersion),
		sf.WithRoundTripper(NewRateLimitTransport(nil)),
	)
	if err != nil {
		return nil, fmt.Errorf("salesforce authentication failed: %w", err)
	}

	slog.Info("Salesforce client authenticated",
		"auth_flow", client.GetAuthFlow(),
		"api_version", client.GetAPIVersion(),
		"instance_url", client.GetInstanceUrl(),
	)

	return client, nil
}
