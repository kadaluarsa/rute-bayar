package providerfactory

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
)

// EnvAccountLoader reads gateway credentials from environment variables
// before falling back to the database. On Cloud Run, mount Secret Manager
// secrets as these variables so production keys never sit in SQLite:
//
//	RUTE_BAYAR_ACCOUNT_<PROVIDER>_<ENVIRONMENT>_CREDENTIAL_JSON
//	RUTE_BAYAR_ACCOUNT_<PROVIDER>_<ENVIRONMENT>_CONFIG_JSON (optional)
//
// e.g. RUTE_BAYAR_ACCOUNT_MIDTRANS_PRODUCTION_CREDENTIAL_JSON holds the
// same JSON that `rutebayar onboard midtrans` would store.
type EnvAccountLoader struct {
	Fallback AccountLoader
	Lookup   func(string) string
}

func NewEnvAccountLoader(fallback AccountLoader) *EnvAccountLoader {
	return &EnvAccountLoader{Fallback: fallback, Lookup: os.Getenv}
}

func EnvAccountVariable(providerCode domain.ProviderCode, environment domain.Environment, suffix string) string {
	return "RUTE_BAYAR_ACCOUNT_" + strings.ToUpper(string(providerCode)) + "_" + strings.ToUpper(string(environment)) + "_" + suffix
}

func (l *EnvAccountLoader) GetProviderAccount(ctx context.Context, providerCode domain.ProviderCode, environment domain.Environment) (domain.ProviderAccount, error) {
	lookup := l.Lookup
	if lookup == nil {
		lookup = os.Getenv
	}
	credential := strings.TrimSpace(lookup(EnvAccountVariable(providerCode, environment, "CREDENTIAL_JSON")))
	if credential != "" && json.Valid([]byte(credential)) {
		cfg := strings.TrimSpace(lookup(EnvAccountVariable(providerCode, environment, "CONFIG_JSON")))
		if cfg == "" || !json.Valid([]byte(cfg)) {
			cfg = "{}"
		}
		now := time.Now().UTC()
		return domain.ProviderAccount{
			ID:             "env:" + string(providerCode) + ":" + string(environment),
			ProviderCode:   providerCode,
			Environment:    environment,
			DisplayName:    string(providerCode) + " (environment)",
			CredentialJSON: json.RawMessage(credential),
			ConfigJSON:     json.RawMessage(cfg),
			CreatedAt:      now,
			UpdatedAt:      now,
		}, nil
	}
	return l.Fallback.GetProviderAccount(ctx, providerCode, environment)
}
