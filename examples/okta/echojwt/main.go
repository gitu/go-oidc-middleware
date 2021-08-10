package main

import (
	"examples/shared"
	"fmt"
	"os"

	"github.com/xenitab/go-oidc-middleware/oidcechojwt"
)

func main() {
	cfg, err := shared.NewOktaConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	err = run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "application returned error: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg shared.OktaConfig) error {
	parseToken := oidcechojwt.New(&oidcechojwt.Options{
		Issuer:                     cfg.Issuer,
		FallbackSignatureAlgorithm: cfg.FallbackSignatureAlgorithm,
		RequiredClaims: map[string]interface{}{
			"cid": cfg.ClientID,
		},
	})

	return shared.RunEchoJWT(parseToken, cfg.Address, cfg.Port)
}