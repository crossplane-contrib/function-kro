//go:build generate
// +build generate

package generate

//go:generate rm -f kro/graph/schema/resolver/generated/zz_generated.openapi.go
//go:generate go run -tags generate k8s.io/kube-openapi/cmd/openapi-gen --output-dir ../kro/graph/schema/resolver/generated/ --output-file zz_generated.openapi.go --output-pkg "github.com/crossplane-contrib/function-kro/kro/graph/schema/resolver/generated" k8s.io/api/core/v1 k8s.io/api/apps/v1 k8s.io/api/batch/v1 k8s.io/api/rbac/v1 k8s.io/api/networking/v1 k8s.io/api/policy/v1 k8s.io/api/storage/v1 k8s.io/api/autoscaling/v2 k8s.io/api/coordination/v1 k8s.io/apimachinery/pkg/apis/meta/v1 k8s.io/apimachinery/pkg/runtime k8s.io/apimachinery/pkg/api/resource k8s.io/apimachinery/pkg/util/intstr
