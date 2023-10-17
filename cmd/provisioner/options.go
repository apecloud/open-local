package provisioner

import (
	"github.com/spf13/pflag"
)

type provisionerOption struct {
	BasePath string
}

func (option *provisionerOption) addFlags(fs *pflag.FlagSet) {
	fs.StringVar(&option.BasePath, "basePath", "", "the local base path to host local pv")
}
