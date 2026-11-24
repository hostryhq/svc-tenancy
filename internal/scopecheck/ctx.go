package scopecheck

import (
	"context"

	"github.com/hostryhq/go-common/tenancy"
)

// tenantCtx is the context of a service-token call naming the harness's tenant (what identity sends).
func tenantCtx() context.Context {
	return tenancy.With(context.Background(), tenancy.Context{TenantID: World.Org, ActorID: "identity"})
}
