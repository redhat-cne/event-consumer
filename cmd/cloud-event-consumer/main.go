// Copyright 2020 The Cloud Native Events Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ce "github.com/cloudevents/sdk-go/v2/event"
	"github.com/redhat-cne/event-consumer/pkg/auth"
	"github.com/redhat-cne/event-consumer/pkg/common"
	"github.com/redhat-cne/event-consumer/pkg/restclient"
	ptpEvent "github.com/redhat-cne/sdk-go/pkg/event/ptp"
	redfishEvent "github.com/redhat-cne/sdk-go/pkg/event/redfish"
	"github.com/redhat-cne/sdk-go/pkg/pubsub"
	"github.com/redhat-cne/sdk-go/pkg/types"
	v1pubsub "github.com/redhat-cne/sdk-go/v1/pubsub"
	log "github.com/sirupsen/logrus"
)

// ConsumerTypeEnum enum to choose consumer type
type ConsumerTypeEnum string

var consumerType ConsumerTypeEnum

const (
	// PTP consumer
	PTP ConsumerTypeEnum = "PTP"
	// HW Consumer
	HW ConsumerTypeEnum = "HW"
	// MOCK consumer
	MOCK                      ConsumerTypeEnum = "MOCK"
	subscriptionRetryInterval                  = 30 * time.Second
	// eventPullInterval interval in seconds for pulling events
	eventPullInterval = 60
	defaultAPIAddr    = "localhost:8089"
)

var (
	apiAddr            string
	apiPath            = "/api/ocloudNotifications/v2/"
	apiVersion         string
	localAPIAddr       = "localhost:8989"
	resourcePrefix     = "/cluster/node/%s%s"
	mockResource       = "/mock"
	mockResourceKey    = "mock"
	httpEventPublisher string
	authConfigPath     string
	// subs tracks the individual event resources and their subscription IDs.
	subs []*pubsub.PubSub
	// Git commit of current build set at build time
	GitCommit = "Undefined"
	// Global authenticated REST client
	authenticatedClient *restclient.Rest
	// Global authentication configuration
	authConfig *auth.AuthConfig
	// callbackTokenValidator validates bearer tokens presented by the publisher
	// when it pushes events to this consumer's callback endpoints (OAuth mode).
	callbackTokenValidator auth.TokenValidator
)

func main() {
	fmt.Printf("Git commit: %s\n", GitCommit)
	common.InitLogger()
	subscribe := flag.Bool("subscribe", true, "Automatically create and retry subscriptions")
	flag.StringVar(&localAPIAddr, "local-api-addr", "localhost:8989", "The address the local api binds to .")
	flag.StringVar(&apiPath, "api-path", "/api/ocloudNotifications/v2/", "The rest api path.")
	flag.StringVar(&apiAddr, "api-addr", "", "Obsolete. The publisher API address is retrieved from httpEventPublisher flag")
	flag.StringVar(&apiVersion, "api-version", "", "Obsolete. The version of event REST API is set to 2.0.")
	flag.StringVar(&httpEventPublisher, "http-event-publishers", "", "Comma separated address of the publishers available.")
	flag.StringVar(&authConfigPath, "auth-config", "", "Path to authentication configuration file (JSON format).")
	flag.Parse()

	if apiAddr != "" {
		log.Warn("api-addr flag is obsolete. Publisher API address is retrieved from httpEventPublisher flag")
	}

	if apiVersion != "" {
		log.Warn("api-version flag is obsolete. Event REST API version is set to 2.0")
	}

	// Initialize authentication
	if err := initializeAuthentication(); err != nil {
		log.Fatalf("Failed to initialize authentication: %v", err)
	}

	nodeIP := os.Getenv("NODE_IP")
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Error("cannot find NODE_NAME environment variable,setting to default `mock` node")
		nodeName = mockResourceKey
	}

	enableEventPull := common.GetBoolEnv("ENABLE_STATUS_CHECK")

	consumerTypeEnv := os.Getenv("CONSUMER_TYPE")
	if consumerTypeEnv == "" {
		consumerType = MOCK
	} else {
		consumerType = ConsumerTypeEnum(consumerTypeEnv)
	}

	// get the first publisher and replace the apiAddr
	apiAddr = getFirstHTTPPublishers(nodeIP, nodeName, httpEventPublisher)
	if apiAddr == "" {
		log.Errorf("cannot find publisher, setting to default address %s", defaultAPIAddr)
		apiAddr = defaultAPIAddr
	}
	log.Infof("updated apiAddr=%s, apiPath=%s", apiAddr, apiPath)

	subscribeTo := initSubscribers(consumerType)
	for _, resource := range subscribeTo {
		subs = append(subs, &pubsub.PubSub{
			Resource: fmt.Sprintf(resourcePrefix, nodeName, resource),
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := newCallbackServer()
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatalf("listen for event callbacks: %v", err)
	}
	defer listener.Close()
	go func() {
		if serveErr := serveCallbacks(server, listener); serveErr != nil && serveErr != http.ErrServerClosed {
			log.Errorf("event callback server: %v", serveErr)
			stop()
		}
	}()

	log.Info("waiting for events")
	runEventLoop(ctx, *subscribe, enableEventPull)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Errorf("shutdown event callback server: %v", err)
	}
}

func runEventLoop(ctx context.Context, subscribe, enableEventPull bool) {
	if !subscribe {
		<-ctx.Done()
		return
	}
	conflicts := make(map[string]bool)
	for !subscribeToEvents(ctx, conflicts) {
		timer := time.NewTimer(subscriptionRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	if !enableEventPull {
		<-ctx.Done()
		return
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			pullEvents(ctx)
			timer.Reset(eventPullInterval * time.Second)
		}
	}
}

// initializeAuthentication initializes the authentication configuration and client
func initializeAuthentication() error {
	if authConfigPath == "" {
		log.Info("No authentication configuration provided, using basic HTTP client")
		authenticatedClient = restclient.New()
		authConfig = nil
		return nil
	}

	// Load authentication configuration
	var err error
	authConfig, err = auth.LoadAuthConfig(authConfigPath)
	if err != nil {
		return fmt.Errorf("failed to load authentication configuration: %v", err)
	}

	// Print authentication configuration summary
	log.Info(authConfig.GetConfigSummary())

	// Create authenticated REST client
	authenticatedClient, err = restclient.NewAuthenticated(authConfig)
	if err != nil {
		return fmt.Errorf("failed to create authenticated REST client: %v", err)
	}

	// When OAuth is enabled the publisher pushes events to this consumer's
	// callback carrying a bearer token; build the validator that verifies it via
	// the Kubernetes TokenReview API. The consumer ServiceAccount must be bound
	// to system:auth-delegator (see manifests/auth/rbac.yaml).
	if authConfig.EnableOAuth {
		callbackTokenValidator, err = auth.NewTokenReviewValidator()
		if err != nil {
			return fmt.Errorf("failed to create TokenReview validator for callback authentication: %v", err)
		}
	}

	log.Info("Authentication initialized successfully")
	return nil
}

// callbackAuthMiddleware enforces on the consumer's inbound callback endpoints
// (/event, /ack/event) the same authentication the publisher enforces on its
// APIs: a same-pod loopback caller is trusted, otherwise every enabled mechanism
// must pass - a verified client certificate when mTLS is enabled and a valid
// bearer token (Kubernetes TokenReview) when OAuth is enabled. This closes the
// O-RAN CR-0003 clause 4.1.1 gap for a consumer in a separate POD/VM: the pushed
// CloudEvent is authenticated, not merely transported over TLS. It mirrors
// rest-api's combinedAuthMiddleware, so CreateServerTLSConfig can keep
// VerifyClientCertIfGiven (allowing OAuth-only and loopback) while presented
// certificates are still required here when mTLS is on.
func callbackAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// No authentication configured: plain-HTTP callback, pass through.
		if authConfig == nil || (!authConfig.EnableMTLS && !authConfig.EnableOAuth) {
			next.ServeHTTP(w, req)
			return
		}
		// Trusted loopback fast-path (same-pod producer self-call).
		if isLoopbackRemoteAddr(req.RemoteAddr) {
			next.ServeHTTP(w, req)
			return
		}
		// mTLS: require a client certificate the TLS handshake already verified
		// against the configured CA pool.
		if authConfig.EnableMTLS {
			if req.TLS == nil || len(req.TLS.VerifiedChains) == 0 {
				log.Warnf("mTLS required but no client certificate provided for %s", req.URL.Path)
				http.Error(w, "Client certificate required", http.StatusUnauthorized)
				return
			}
		}
		// OAuth: require and validate a bearer token.
		if authConfig.EnableOAuth {
			authHeader := req.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				log.Warnf("OAuth required but no bearer token provided for %s", req.URL.Path)
				http.Error(w, "Bearer token required", http.StatusUnauthorized)
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			// Fail closed: OAuth is enabled but no validator was installed.
			if callbackTokenValidator == nil {
				log.Error("OAuth enabled but no TokenReview validator configured; rejecting callback")
				http.Error(w, "token validation unavailable", http.StatusUnauthorized)
				return
			}
			if _, err := callbackTokenValidator.ValidateToken(req.Context(), token, authConfig.RequiredAudiences); err != nil {
				log.Warnf("OAuth token validation failed for %s: %v", req.URL.Path, err)
				http.Error(w, "Invalid OAuth token", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

// isLoopbackRemoteAddr reports whether an http.Request RemoteAddr is a loopback
// address (a same-pod caller), which is trusted and exempt from callback auth.
func isLoopbackRemoteAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// getScheme returns the appropriate URL scheme based on authentication configuration.
// HTTPS is required whenever a credential is sent on the wire: mTLS presents a client
// certificate, and OAuth sends a bearer token that must never traverse cleartext HTTP
// (CWE-319). Returning "http" only when no authentication is configured.
func getScheme() string {
	if authConfig != nil && (authConfig.EnableMTLS || authConfig.EnableOAuth) {
		return "https"
	}
	return "http"
}

func subscribeToEvents(ctx context.Context, conflicts map[string]bool) bool {
	allSubscribed := true
	for _, sub := range subs {
		if ctx.Err() != nil {
			return false
		}
		if sub.ID != "" || conflicts[sub.Resource] {
			continue
		}
		created, status, err := createSubscription(ctx, sub.Resource)
		if status == http.StatusConflict {
			conflicts[sub.Resource] = true
			log.Infof("Subscription already exists for %s", sub.Resource)
			status, body, err := authenticatedClient.GetContext(ctx, subscriptionsURL())
			if err != nil || status != http.StatusOK {
				log.Warnf("Could not list current subscriptions: HTTP %d (error: %v)", status, err)
			} else {
				log.Infof("Current subscriptions: %s", body)
			}
			continue
		}
		if err != nil {
			log.Errorf("failed to create subscription: %v", err)
			allSubscribed = false
			continue
		}
		if created.ID == "" {
			log.Errorf("publisher returned no subscription ID for %s", sub.Resource)
			allSubscribed = false
			continue
		}
		sub.ID = created.ID
		log.Infof("created subscription: %s", created.String())
	}
	return allSubscribed
}

func subscriptionsURL() *types.URI {
	return &types.URI{URL: url.URL{Scheme: getScheme(),
		Host: apiAddr,
		Path: apiPath + "subscriptions"}}
}

func createSubscription(ctx context.Context, resourceAddress string) (sub pubsub.PubSub, status int, err error) {
	subURL := subscriptionsURL()
	// Register the callback EndpointURI with the same scheme used for outbound
	// calls: https:// when authentication is enabled. Per O-RAN CR-0003 clause
	// 4.1.1 a callback reachable from a separate POD/VM must be protected by an
	// authorization mechanism (clause 3.2); registering a plaintext http://
	// callback would have the producer push event data in the clear (CWE-319)
	// and accept unauthenticated, spoofable events.
	endpointURL := &types.URI{URL: url.URL{Scheme: getScheme(),
		Host: localAPIAddr,
		Path: "event"}}

	sub = v1pubsub.NewPubSub(endpointURL, resourceAddress)
	var subB []byte

	if subB, err = json.Marshal(&sub); err == nil {
		status, subB = authenticatedClient.PostWithReturnContext(ctx, subURL, subB)
		if status == http.StatusCreated {
			err = json.Unmarshal(subB, &sub)
		} else {
			err = fmt.Errorf("api at %s returned status %d for %s", subURL, status, resourceAddress)
		}
	} else {
		err = fmt.Errorf("failed to marshal subscription for %s", resourceAddress)
	}
	return
}

// getCurrentState get event state for the resource
func getCurrentState(ctx context.Context, resource string) error {
	//create publisher
	url := &types.URI{URL: url.URL{Scheme: getScheme(),
		Host: apiAddr,
		Path: fmt.Sprintf("%s%s", apiPath, fmt.Sprintf("%s/CurrentState", resource[1:]))}}
	status, cloudEvent, err := authenticatedClient.GetContext(ctx, url)
	if status != http.StatusOK {
		if err != nil {
			log.Error(err)
		}
		return fmt.Errorf("CurrentState: error %d from url %s", status, url.String())
	} else {
		log.Debugf("Got CurrentState: %s", cloudEvent)
	}
	return nil
}

// Consumer webserver
func newCallbackServer() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/event", getEvent)
	mux.HandleFunc("/ack/event", ackEvent)

	port := extractPort(localAPIAddr)
	server := &http.Server{
		Addr:              port,
		ReadHeaderTimeout: 3 * time.Second,
		// Authenticate pushed callbacks (mTLS client cert and/or OAuth bearer
		// token) when auth is enabled; a no-op pass-through otherwise.
		Handler: callbackAuthMiddleware(mux),
	}

	// When authentication is enabled the callback EndpointURI is registered as
	// https:// (see getScheme/createSubscription), so the local /event server
	// must terminate TLS and authenticate the pushing producer. This mirrors the
	// control/pull path and closes the CR-0003 clause 4.1.1 gap for consumers in
	// a separate POD/VM.
	if getScheme() == "https" {
		tlsConfig, err := authConfig.CreateServerTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("create TLS config for callback server: %w", err)
		}
		server.TLSConfig = tlsConfig
	}
	return server, nil
}

func serveCallbacks(server *http.Server, listener net.Listener) error {
	if server.TLSConfig != nil {
		log.Infof("Starting local API (HTTPS, mTLS) listening on %s", listener.Addr())
		// The certificate and key are supplied in the TLS config.
		return server.ServeTLS(listener, "", "")
	}
	log.Infof("Starting local API listening on %s", listener.Addr())
	return server.Serve(listener)
}

func getEvent(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		log.Errorf("error reading event %v", err)
	}
	e := string(bodyBytes)
	if e != "" {
		if err = processEvent(bodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func ackEvent(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		log.Errorf("error reading acknowledgment  %v", err)
	}
	e := string(bodyBytes)
	if e != "" {
		log.Infof("received ack %s", string(bodyBytes))
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func processEvent(data []byte) error {
	var e ce.Event
	if err := json.Unmarshal(data, &e); err != nil {
		log.Errorf("failed to unmarshal event, %v", err)
		return err
	}
	// Microsecond resolution: the publisher->consumer hop is in-node (localhost)
	// on an SNO, so per-event auth overhead is sub-millisecond and invisible at
	// ms granularity. Log µs (and keep a ms figure for readability) so the
	// baseline-vs-mTLS/OAuth A/B delta is measurable.
	latencyUs := time.Now().UnixMicro() - e.Context.GetTime().UnixMicro()
	// set log to Info level for performance measurement
	log.Infof("Latency for the event: %d us (%.3f ms)", latencyUs, float64(latencyUs)/1000.0)
	log.Infof("received event %s: %s", e.ID(), data)
	return nil
}

func initSubscribers(cType ConsumerTypeEnum) map[string]string {
	subscribeTo := make(map[string]string)
	switch cType {
	case PTP:
		subscribeTo[string(ptpEvent.OsClockSyncStateChange)] = string(ptpEvent.OsClockSyncState)
		subscribeTo[string(ptpEvent.PtpClockClassChange)] = string(ptpEvent.PtpClockClass)
		subscribeTo[string(ptpEvent.PtpStateChange)] = string(ptpEvent.PtpLockState)
		subscribeTo[string(ptpEvent.GnssStateChange)] = string(ptpEvent.GnssSyncStatus)
		subscribeTo[string(ptpEvent.SyncStateChange)] = string(ptpEvent.SyncStatusState)
	case MOCK:
		subscribeTo[mockResourceKey] = mockResource
	case HW:
		subscribeTo[string(redfishEvent.Alert)] = string(redfishEvent.Systems)
		subscribeTo[string(redfishEvent.ResourceAdded)] = string(redfishEvent.Systems)
		subscribeTo[string(redfishEvent.ResourceUpdated)] = string(redfishEvent.Systems)
		subscribeTo[string(redfishEvent.ResourceRemoved)] = string(redfishEvent.Systems)
		subscribeTo[string(redfishEvent.StatusChange)] = string(redfishEvent.Systems)
	default:
		subscribeTo[mockResourceKey] = mockResource
	}
	return subscribeTo
}

func pullEvents(ctx context.Context) {
	for _, s := range subs {
		if ctx.Err() != nil {
			return
		}
		if err := getCurrentState(ctx, s.Resource); err != nil && ctx.Err() == nil {
			log.Error(err)
		}
	}
}

func getFirstHTTPPublishers(nodeIP, nodeName string, addr ...string) string {
	for _, s := range addr {
		if s == "" {
			continue
		}
		return common.SanitizeTransportHost(s, nodeIP, nodeName)
	}
	return ""
}

// extract port from host:port and make sure to support ipv6
func extractPort(hostPort string) string {
	_, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	return ":" + port
}
