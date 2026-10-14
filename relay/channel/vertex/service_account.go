package vertex

import (
	"context"
	"errors"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
)

type Credentials = service.VertexCredentials

func getAccessToken(a *Adaptor, info *relaycommon.RelayInfo) (string, error) {
	settings := info.ChannelSetting
	client, err := service.VertexOAuthHTTPClient(settings.Proxy, settings)
	if err != nil {
		return "", errors.New("Vertex service account authentication failed")
	}
	defer client.CloseIdleConnections()
	return service.VertexAccessToken(context.Background(), a.AccountCredentials, client)
}

// AcquireAccessToken remains available to task plugins and existing callers.
func AcquireAccessToken(creds Credentials, proxy string) (string, error) {
	client, err := service.VertexOAuthHTTPClient(proxy, dto.ChannelSettings{})
	if err != nil {
		return "", errors.New("Vertex service account authentication failed")
	}
	defer client.CloseIdleConnections()
	return service.VertexAccessToken(context.Background(), creds, client)
}
