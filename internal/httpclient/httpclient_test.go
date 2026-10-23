package httpclient_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/nostosx/agentx/internal/httpclient"
)

func TestAddHeaders(t *testing.T) {
	t.Parallel()
	dst := http.Header{"X-Test": {"existing"}}
	src := http.Header{"X-Test": {"first", "second"}, "X-Other": {"value"}}
	httpclient.AddHeaders(dst, src)
	want := http.Header{"X-Test": {"existing", "first", "second"}, "X-Other": {"value"}}
	if !reflect.DeepEqual(dst, want) {
		t.Fatalf("headers = %v, want %v", dst, want)
	}
	src["X-Other"][0] = "changed"
	if dst.Get("X-Other") != "value" {
		t.Fatal("destination aliases source values")
	}
	httpclient.AddHeaders(dst, nil)
	if !reflect.DeepEqual(dst, want) {
		t.Fatal("nil source changed destination")
	}
}

func TestClientOr(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	if httpclient.ClientOr(client) != client {
		t.Fatal("configured client replaced")
	}
	if httpclient.ClientOr(nil) != http.DefaultClient {
		t.Fatal("default client not selected")
	}
}
