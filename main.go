// Package main implements a Composition Function.
package main

import (
	"github.com/alecthomas/kong"

	"github.com/crossplane/function-sdk-go"

	"github.com/crossplane-contrib/function-kro/kro/features"
	"github.com/crossplane-contrib/function-kro/kro/graph"
)

// CLI of this Function.
type CLI struct {
	Debug bool `help:"Emit debug logs in addition to info logs." short:"d"`

	Network            string `default:"tcp"                                                                                        help:"Network on which to listen for gRPC connections."`
	Address            string `default:":9443"                                                                                      help:"Address at which to listen for gRPC connections."`
	TLSCertsDir        string `env:"TLS_SERVER_CERTS_DIR"                                                                           help:"Directory containing server certs (tls.key, tls.crt) and the CA used to verify client certificates (ca.crt)"`
	Insecure           bool   `help:"Run without mTLS credentials. If you supply this flag --tls-server-certs-dir will be ignored."`
	MaxRecvMessageSize int    `default:"4"                                                                                          help:"Maximum size of received messages in MB."`

	FeatureGates            string `default:""     help:"Feature gates to enable/disable (e.g. CELOmitFunction=true)."`
	RGDMaxCollectionSize    int    `default:"1000" help:"Maximum number of resources in a single collection."`
	RGDMaxCollectionDimSize int    `default:"10"   help:"Maximum number of dimensions in a forEach."`
}

// Run this Function.
func (c *CLI) Run() error {
	log, err := function.NewLogger(c.Debug)
	if err != nil {
		return err
	}

	if c.FeatureGates != "" {
		if err := features.FeatureGate.Set(c.FeatureGates); err != nil {
			return err
		}
	}

	rgdConfig := graph.RGDConfig{
		MaxCollectionSize:          c.RGDMaxCollectionSize,
		MaxCollectionDimensionSize: c.RGDMaxCollectionDimSize,
	}

	return function.Serve(&Function{log: log, rgdConfig: rgdConfig},
		function.Listen(c.Network, c.Address),
		function.MTLSCertificates(c.TLSCertsDir),
		function.Insecure(c.Insecure),
		function.MaxRecvMessageSize(c.MaxRecvMessageSize*1024*1024))
}

func main() {
	ctx := kong.Parse(&CLI{}, kong.Description("A Crossplane Composition Function."))
	ctx.FatalIfErrorf(ctx.Run())
}
