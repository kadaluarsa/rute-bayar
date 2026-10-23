package providerfactory

import (
	"context"
	"errors"
	"testing"

	"github.com/pendig/rute-bayar/internal/domain"
)

type noAccounts struct{}

func (noAccounts) GetProviderAccount(context.Context, domain.ProviderCode, domain.Environment) (domain.ProviderAccount, error) {
	return domain.ProviderAccount{}, errors.New("not in db")
}

func TestEnvAccountLoaderPrefersEnvironment(t *testing.T) {
	env := map[string]string{"RUTE_BAYAR_ACCOUNT_MIDTRANS_PRODUCTION_CREDENTIAL_JSON": `{"server_key":"sk"}`}
	loader := &EnvAccountLoader{Fallback: noAccounts{}, Lookup: func(k string) string { return env[k] }}
	account, err := loader.GetProviderAccount(context.Background(), domain.ProviderMidtrans, domain.EnvironmentProduction)
	if err != nil || string(account.CredentialJSON) != `{"server_key":"sk"}` || string(account.ConfigJSON) != "{}" {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	if _, err := loader.GetProviderAccount(context.Background(), domain.ProviderXendit, domain.EnvironmentProduction); err == nil {
		t.Fatal("missing env should fall back to the database loader")
	}
}
