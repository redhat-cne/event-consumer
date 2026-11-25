package restclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/redhat-cne/sdk-go/pkg/types"
)

func TestSubscriptionRetryUsesFreshTLSConnectionAfterUnauthorized(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
		}
		fmt.Fprint(w, r.RemoteAddr)
	}))
	defer server.Close()

	client, err := NewAuthenticated(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the TLS transport that trusts the test server.
	client.authClient.GetClient().Transport = server.Client().Transport
	endpoint, err := url.Parse(server.URL + "/subscriptions")
	if err != nil {
		t.Fatal(err)
	}
	subscriptions := &types.URI{URL: *endpoint}
	firstStatus, first := client.PostWithReturn(subscriptions, nil)
	secondStatus, second := client.PostWithReturn(subscriptions, nil)
	thirdStatus, third := client.PostWithReturn(subscriptions, nil)
	if firstStatus != http.StatusUnauthorized || secondStatus != http.StatusOK || thirdStatus != http.StatusOK {
		t.Fatalf("unexpected statuses: %d, %d, %d", firstStatus, secondStatus, thirdStatus)
	}
	if string(first) == string(second) {
		t.Fatal("the retry reused the TLS connection that received a 401")
	}
	if string(second) != string(third) {
		t.Fatal("a successful request should keep its TLS connection reusable")
	}
}
