package panel

import (
	"net/url"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestOperatorProfiles(t *testing.T) {
	ctx := t.Context()
	db := filepath.Join(t.TempDir(), "panel.sqlite")
	reg, err := OpenRegistry(ctx, "sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	base, _ := url.Parse("https://sub.example.org")
	reality := &Reality{realityEndpoint: realityEndpoint{Public: "vpn.example.org:8443"}, ServerName: "panel.example.org"}
	if err := reg.InitializeReality(ctx, reality); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ProxyURL: base, Reality: reality}
	routes := []OperatorRoute{
		{Operator: "MCI", Transport: "reality"},
		{Operator: "Irancell", Transport: "ws"},
		{Operator: "Shatel", Transport: "ws", Endpoint: "https://other.example.org"},
	}
	valid, err := validateOperatorRoutes(routes, cfg)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append(slices.Clone(routes), OperatorRoute{Operator: "Backup", Transport: "ws", Endpoint: "https://sub.example.org:443/"})
	if _, err := validateOperatorRoutes(duplicate, cfg); err == nil {
		t.Fatal("accepted a duplicate connection endpoint")
	}
	if err := reg.saveOperatorRoutes(ctx, valid); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenRegistry(ctx, "sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	account := User{ID: uuid.New(), Username: "alex", Remark: "Home + Studio", Quota: 10_000_000_000}
	profiles := subscriptionProfiles(reloaded, base, reality, account)
	expected := []struct{ label, address, security, path string }{
		{"Home + Studio (MCI)", "vpn.example.org:8443", "reality", ""},
		{"Home + Studio (Irancell)", "sub.example.org:443", "tls", PrimaryPath},
		{"Home + Studio (Shatel)", "other.example.org:443", "tls", PrimaryPath},
	}
	if len(profiles) != len(expected) {
		t.Fatalf("got %d profiles, expected %d", len(profiles), len(expected))
	}
	for index, want := range expected {
		link, err := url.Parse(profiles[index].URI)
		if err != nil {
			t.Fatal(err)
		}
		if link.User.Username() != account.ID.String() || link.Fragment != want.label || link.Host != want.address ||
			link.Query().Get("security") != want.security || link.Query().Get("path") != want.path {
			t.Fatalf("profile %d did not use the expected label, address and settings", index)
		}
	}
	if withoutReality := subscriptionProfiles(reloaded, base, nil, account); len(withoutReality) != 2 {
		t.Fatal("disabled REALITY route remained exposed")
	}
	if err := reg.saveOperatorRoutes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if profiles := subscriptionProfiles(reg, base, reality, account); len(profiles) != 2 {
		t.Fatal("clearing operator routes did not restore default profiles")
	}
}
