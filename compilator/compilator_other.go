//go:build !linux
// +build !linux

package compilator

import (
	"fmt"

	"github.com/vikramraodp/fissile/model"
)

func (c *Compilator) compilePackageInMountNS(pkg *model.Package) (err error) {
	return fmt.Errorf("Compilation without docker is not supported outside Linux")
}
