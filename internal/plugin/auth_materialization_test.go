package plugin

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/config"
)

func TestFindMaterializationTargetRejectsAmbiguousOwnership(t *testing.T) {
	id := config.APIKeyCredentialID("materialization-test")
	name := id + ".json"
	valid := pluginapi.HostAuthFileEntry{ID: id, Name: name, AuthIndex: "auth-index", Type: ProviderID, Path: `C:\auth\` + name}
	cases := []struct {
		name    string
		entries []pluginapi.HostAuthFileEntry
	}{
		{name: "different name", entries: []pluginapi.HostAuthFileEntry{{ID: id, Name: "different.json", AuthIndex: "auth-index", Type: ProviderID, Path: valid.Path}}},
		{name: "wrong provider", entries: []pluginapi.HostAuthFileEntry{{ID: id, Name: name, AuthIndex: "auth-index", Type: "claude", Path: valid.Path}}},
		{name: "runtime only", entries: []pluginapi.HostAuthFileEntry{{ID: id, Name: name, AuthIndex: "auth-index", Type: ProviderID, Path: valid.Path, RuntimeOnly: true}}},
		{name: "missing path", entries: []pluginapi.HostAuthFileEntry{{ID: id, Name: name, AuthIndex: "auth-index", Type: ProviderID}}},
		{name: "missing auth index", entries: []pluginapi.HostAuthFileEntry{{ID: id, Name: name, Type: ProviderID, Path: valid.Path}}},
		{name: "duplicate physical identity", entries: []pluginapi.HostAuthFileEntry{valid, {ID: id, Name: name, AuthIndex: "auth-index-2", Type: ProviderID, Path: `C:\auth\other.json`}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, found, err := findMaterializationTarget(tc.entries, id, name); err == nil || found {
				t.Fatalf("ambiguous target result found=%t err=%v", found, err)
			}
		})
	}
	if _, found, err := findMaterializationTarget([]pluginapi.HostAuthFileEntry{valid}, id, name); err != nil || !found {
		t.Fatalf("valid target result found=%t err=%v", found, err)
	}
	if _, found, err := findMaterializationTarget([]pluginapi.HostAuthFileEntry{{ID: name, Name: name, AuthIndex: "auth-index", Type: ProviderID, Path: valid.Path}}, id, name); err != nil || !found {
		t.Fatalf("CPA filename-normalized ID target result found=%t err=%v", found, err)
	}
	if _, found, err := findMaterializationTarget(nil, id, name); err != nil || found {
		t.Fatalf("missing target result found=%t err=%v", found, err)
	}
}

func TestReferenceOnlyAuthJSONRejectsWrongOwnerAndPreservesUnknownFields(t *testing.T) {
	const key = "auth-migration-test-key"
	id := config.APIKeyCredentialID(key)
	wrongOwner := []byte(`{"type":"claude","id":"` + id + `","api_key":"` + key + `"}`)
	if _, _, err := referenceOnlyAuthJSON(wrongOwner, key, id); err == nil {
		t.Fatal("migration accepted a different provider")
	}
	wrongID := []byte(`{"type":"opencode-go","id":"wrong","api_key":"` + key + `"}`)
	if _, _, err := referenceOnlyAuthJSON(wrongID, key, id); err == nil {
		t.Fatal("migration accepted an ID mismatch")
	}
	wrongKey := []byte(`{"type":"opencode-go","id":"` + id + `","api_key":"not-the-key"}`)
	if _, _, err := referenceOnlyAuthJSON(wrongKey, key, id); err == nil {
		t.Fatal("migration accepted a legacy key mismatch")
	}
	legacy := []byte(`{"provider":"opencode-go","id":"` + id + `","api_key":"` + key + `","note":{"nested":[1,true,"x"]},"weight":9}`)
	got, changed, err := referenceOnlyAuthJSON(legacy, key, id)
	if err != nil || !changed {
		t.Fatalf("legacy migration changed=%t err=%v", changed, err)
	}
	var record map[string]json.RawMessage
	if json.Unmarshal(got, &record) != nil {
		t.Fatal("migrated JSON is invalid")
	}
	if _, ok := record["api_key"]; ok {
		t.Fatal("migrated JSON retains api_key")
	}
	var kind, label string
	_ = json.Unmarshal(record["type"], &kind)
	_ = json.Unmarshal(record["label"], &label)
	if kind != ProviderID || label == "" {
		t.Fatalf("migrated identity fields type=%q label=%q", kind, label)
	}
	for field, expected := range map[string]any{"note": map[string]any{"nested": []any{float64(1), true, "x"}}, "weight": float64(9)} {
		var actual any
		if json.Unmarshal(record[field], &actual) != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("migrated field %q = %#v, want %#v", field, actual, expected)
		}
	}
}
