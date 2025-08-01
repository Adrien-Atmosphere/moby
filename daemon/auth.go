package daemon

import (
	"context"

	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/dockerversion"
)

// AuthenticateToRegistry checks the validity of credentials in authConfig
func (daemon *Daemon) AuthenticateToRegistry(ctx context.Context, authConfig *registry.AuthConfig) (string, error) {
	_, token, err := daemon.registryService.Auth(ctx, authConfig, dockerversion.DockerUserAgent(ctx))
	return token, err
}
