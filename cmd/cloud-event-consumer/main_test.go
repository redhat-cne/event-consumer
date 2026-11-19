package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redhat-cne/event-consumer/pkg/auth"
	"github.com/redhat-cne/event-consumer/pkg/restclient"
)

type callbackTestValidator struct{}

func (callbackTestValidator) ValidateToken(_ context.Context, token string, audiences []string) (*auth.TokenInfo, error) {
	if token != "valid" || len(audiences) != 1 || audiences[0] != "event-api" {
		return nil, fmt.Errorf("invalid token")
	}
	return &auth.TokenInfo{Username: "publisher"}, nil
}

func TestCallbackRequiresEnabledCredentials(t *testing.T) {
	oldConfig, oldValidator := authConfig, callbackTokenValidator
	defer func() { authConfig, callbackTokenValidator = oldConfig, oldValidator }()
	authConfig = &auth.AuthConfig{ServerConfig: &auth.ServerConfig{EnableMTLS: true, EnableOAuth: true, RequiredAudiences: []string{"event-api"}}}
	callbackTokenValidator = callbackTestValidator{}
	for _, tc := range []struct {
		name, token string
		verified    bool
		want        int
	}{
		{"none", "", false, 401},
		{"certificate only", "", true, 401},
		{"token only", "valid", false, 401},
		{"invalid token", "invalid", true, 401},
		{"both", "valid", true, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/event", nil)
			r.RemoteAddr = "192.0.2.1:1234"
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			r.Header.Set("Authorization", "Bearer "+tc.token)
			// Presence alone is insufficient; require the verified chain.
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
			if tc.verified {
				r.TLS.VerifiedChains = [][]*x509.Certificate{{{}}}
			}
			w := httptest.NewRecorder()
			callbackAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
		})
	}
}

func TestPrettyEventJSON(t *testing.T) {
	raw := []byte(` {"specversion":"1.0","id":"event-1","data":{"value":42}} `)
	want := "{\n  \"specversion\": \"1.0\",\n  \"id\": \"event-1\",\n  \"data\": {\n    \"value\": 42\n  }\n}"
	if got := prettyEventJSON(raw); got != want {
		t.Fatalf("pretty event:\n%s\nwant:\n%s", got, want)
	}

	invalid := []byte("not-json")
	if got := prettyEventJSON(invalid); got != string(invalid) {
		t.Fatalf("invalid JSON changed: %q", got)
	}
}

// CEPv2 uses the same O-RAN v2 subscription wire contract as CEPv1.
func TestCEPv2SubscriptionWireCompatibility(t *testing.T) {
	const resource = "/cluster/node/test/sync/ptp-status/lock-state"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/ocloudNotifications/v2/subscriptions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var request struct {
			Endpoint string `json:"EndpointUri"`
			Resource string `json:"ResourceAddress"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Resource != resource || request.Endpoint != "http://consumer.example:9043/event" {
			t.Errorf("unexpected subscription: %+v", request)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"SubscriptionId": "test-id", "EndpointUri": request.Endpoint, "ResourceAddress": request.Resource, "UriLocation": "http://publisher.example:9043/api/ocloudNotifications/v2/subscriptions/test-id"})
	}))
	defer server.Close()
	oldAddr, oldPath, oldLocal, oldClient, oldAuth := apiAddr, apiPath, localAPIAddr, authenticatedClient, authConfig
	defer func() {
		apiAddr, apiPath, localAPIAddr, authenticatedClient, authConfig = oldAddr, oldPath, oldLocal, oldClient, oldAuth
	}()
	apiAddr = strings.TrimPrefix(server.URL, "http://")
	apiPath = "/api/ocloudNotifications/v2/"
	localAPIAddr = "consumer.example:9043"
	authenticatedClient = restclient.New()
	authConfig = nil
	sub, status, err := createSubscription(resource)
	if err != nil || status != 201 || sub.ID != "test-id" {
		t.Fatalf("subscription: %+v status=%d error=%v", sub, status, err)
	}
}
