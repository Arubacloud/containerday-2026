package main

import (
	"os"

	function "github.com/crossplane/function-sdk-go"
	"github.com/crossplane/function-sdk-go/logging"

	"github.com/Arubacloud/containerday-2026/functions/appenv-deployer/internal/deployer"
	"github.com/Arubacloud/containerday-2026/functions/appenv-deployer/internal/fn"
)

func main() {
	debug := os.Getenv("FUNCTION_DEBUG") == "true"

	log, err := logging.NewLogger(debug)
	if err != nil {
		panic(err)
	}

	srv := fn.NewFunction(log, deployer.NewSSHDeployer())

	if err := function.Serve(srv,
		function.MTLSCertificates(os.Getenv("TLS_SERVER_CERTS_DIR")),
		function.Insecure(os.Getenv("FUNCTION_INSECURE") == "true"),
	); err != nil {
		log.Info("Cannot serve", "error", err)
		os.Exit(1)
	}
}
