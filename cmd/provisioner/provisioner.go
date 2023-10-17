package provisioner

import (
	"log"

	"github.com/spf13/cobra"
)

var (
	opt = provisionerOption{}
)

var Cmd = &cobra.Command{
	Use:   "host path provisioner",
	Short: "command for provisioning local pv",
	Run: func(cmd *cobra.Command, args []string) {
		err := Start(&opt)
		if err != nil {
			log.Fatalf("error :%s, quitting now\n", err.Error())
		}
	},
}

func init() {
	opt.addFlags(Cmd.Flags())
}

// Start will start agent
func Start(opt *provisionerOption) error {
	return nil
}
