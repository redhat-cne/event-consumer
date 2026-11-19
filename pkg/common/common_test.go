package common

import "testing"

func TestNodePublisherAddress(t *testing.T) {
	got := SanitizeTransportHost("ptp-event-publisher-service-NODE_NAME.openshift-ptp.svc.cluster.local:9043", "192.0.2.10", "worker-a.example.com")
	want := "ptp-event-publisher-service-worker-a.openshift-ptp.svc.cluster.local:9043"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
