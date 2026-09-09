package remnawave

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func inventory(t *testing.T) ([]node, []host) {
	t.Helper()
	var nodes []node
	var hosts []host
	if err := json.Unmarshal([]byte(`[{"uuid":"n1","name":"FIN","address":"management.example.com","configProfile":{"activeConfigProfileUuid":"profile","activeInbounds":[{"uuid":"inbound"}]}},{"uuid":"n2","name":"OFF","address":"off.example.com","isDisabled":true}]`), &nodes); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`[{"uuid":"h1","address":"NODE.example.com.","port":443,"nodes":["n1"],"inbound":{"configProfileUuid":"profile","configProfileInboundUuid":"inbound"}}]`), &hosts); err != nil {
		t.Fatal(err)
	}
	return nodes, hosts
}

func TestSelectsClientHostsAndDeduplicates(t *testing.T) {
	nodes, hosts := inventory(t)
	hosts = append(hosts, hosts[0])
	targets, warnings := selectTargets(nodes, hosts)
	if len(targets) != 1 || targets[0].Address != "node.example.com" || len(targets[0].Names) != 1 || len(warnings) != 0 {
		t.Fatal(targets, warnings)
	}
	nodes[0].IsDisabled = true
	if targets, _ := selectTargets(nodes, hosts); len(targets) != 0 {
		t.Fatal("disabled node selected")
	}
	nodes[0].IsDisabled = false
	hosts = hosts[:1]
	hosts[0].Port = 8443
	if targets, _ := selectTargets(nodes, hosts); len(targets) != 0 {
		t.Fatal("unsupported port selected")
	}
	hosts[0].Port = 443
	hosts[0].Nodes = []string{"n2"}
	if targets, _ := selectTargets(nodes, hosts); len(targets) != 0 {
		t.Fatal("host node restriction ignored")
	}
}

func TestResponseEnvelopeAndAuthorization(t *testing.T) {
	nodes, hosts := inventory(t)
	broken := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		if broken {
			fmt.Fprint(w, `{"response":null}`)
			return
		}
		var data any = nodes
		if r.URL.Path == "/api/hosts" {
			data = hosts
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"response": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client := New(server.URL, "secret", server.Client())
	if targets, _, err := client.Discover(context.Background()); err != nil || len(targets) != 1 {
		t.Fatal(targets, err)
	}
	broken = true
	if _, _, err := client.Discover(context.Background()); err == nil {
		t.Fatal("null inventory accepted")
	}
}

func TestNormalizeTarget(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "https://example.com", "example.com:443", "{node_ip}", "x..com", "::1"} {
		if _, err := normalizeTarget(address); err == nil {
			t.Errorf("accepted %s", address)
		}
	}
}
