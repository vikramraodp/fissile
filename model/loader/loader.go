package loader

// split the loader into a separate package. in case we have several release
// resolvers we want to keep `dep ensure` small

import (
	"github.com/vikramraodp/fissile/model"
	"github.com/vikramraodp/fissile/model/releaseresolver"
	"github.com/vikramraodp/fissile/model/resolver"
)

// LoadRoleManifest loads a yaml manifest that details how jobs get grouped into roles
func LoadRoleManifest(manifestFilePath string, options model.LoadRoleManifestOptions) (*model.RoleManifest, error) {
	roleManifest := model.NewRoleManifest()
	err := roleManifest.LoadManifestFromFile(manifestFilePath)
	if err != nil {
		return nil, err
	}

	r := releaseresolver.NewReleaseResolver(manifestFilePath)
	return resolver.NewResolver(roleManifest, r, options).Resolve()
}
