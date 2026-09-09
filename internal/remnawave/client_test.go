package remnawave

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func inventory(t *testing.T) ([]node, []host) {
	t.Helper()
	var nodes []node
	var hosts []host
	require.NoError(t, json.Unmarshal([]byte(`[{"uuid":"n1","name":"FIN","address":"management.example.com","configProfile":{"activeConfigProfileUuid":"profile","activeInbounds":[{"uuid":"inbound"}]}},{"uuid":"n2","name":"OFF","address":"off.example.com","isDisabled":true}]`), &nodes))
	require.NoError(t, json.Unmarshal([]byte(`[{"uuid":"h1","address":"NODE.example.com.","port":443,"nodes":["n1"],"inbound":{"configProfileUuid":"profile","configProfileInboundUuid":"inbound"}}]`), &hosts))
	return nodes, hosts
}

func TestSelectsClientHostsAndDeduplicates(t *testing.T) {
	nodes, hosts := inventory(t)
	hosts = append(hosts, hosts[0])
	targets, warnings := selectTargets(nodes, hosts)
	require.Len(t, targets, 1)
	require.Equal(t, "node.example.com", targets[0].Address)
	require.Equal(t, []string{"FIN"}, targets[0].Names)
	require.Empty(t, warnings)

	nodes[0].IsDisabled = true
	targets, _ = selectTargets(nodes, hosts)
	require.Empty(t, targets, "disabled node selected")
	nodes[0].IsDisabled = false
	hosts = hosts[:1]
	hosts[0].Port = 8443
	targets, _ = selectTargets(nodes, hosts)
	require.Empty(t, targets, "unsupported port selected")
	hosts[0].Port = 443
	hosts[0].Nodes = []string{"n2"}
	targets, _ = selectTargets(nodes, hosts)
	require.Empty(t, targets, "host node restriction ignored")
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
	targets, _, err := client.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, targets, 1)
	broken = true
	_, _, err = client.Discover(t.Context())
	require.Error(t, err, "null inventory accepted")
}

func TestNormalizeTarget(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "https://example.com", "example.com:443", "{node_ip}", "x..com", "::1"} {
		t.Run(address, func(t *testing.T) {
			_, err := normalizeTarget(address)
			require.Error(t, err)
		})
	}
}
