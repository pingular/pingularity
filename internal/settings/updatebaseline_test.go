package settings

import (
	"context"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/store"
)

// shippedValues is a seed matching the shipped defaults for every flag-seedable
// field, so nothing in these tests is flag-backed unless a test overrides it.
func shippedValues() Values {
	return Values{
		Latency: 5 * time.Second, LatencyEnabled: true, Speed: time.Hour,
		Timeout: 3 * time.Second, DownAfter: 2, UpAfter: 1,
		SpeedtestOnReconnect: true, IPv6Mode: "auto",
		Retention: 30 * 24 * time.Hour, SpeedRetention: 365 * 24 * time.Hour,
		DowntimeRetention: 365 * 24 * time.Hour,
	}
}

// openBaseline returns a store plus a controller seeded with def.
func openBaseline(t *testing.T, def Values) (*Controller, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, err := New(context.Background(), st, def)
	if err != nil {
		t.Fatal(err)
	}
	return c, st
}

// The F7 regression: Update diffed against the flag-overlaid IN-MEMORY values,
// so an explicit save of a field whose running value came from a CLI flag wrote
// nothing (old == new) and the value silently reverted once the flag was
// removed. A submitted flag-backed value must persist and survive the flag's
// removal.
func TestUpdatePersistsFlagBackedValue(t *testing.T) {
	ctx := context.Background()
	def := shippedValues()
	def.Timeout = 9 * time.Second // what `-timeout 9s` seeds; shipped default is 3s
	c, st := openBaseline(t, def)

	// The UI form echoes the running value back on Save.
	if _, err := c.Update(ctx, Patch{Timeout: pv(9 * time.Second)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	kv, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if kv[keyTimeout] != "9" {
		t.Fatalf("timeout_s = %q, want \"9\" (an explicit save of a flag-backed value must persist it)", kv[keyTimeout])
	}
	// The saved value now survives a restart WITHOUT the flag.
	c2, err := New(ctx, st, shippedValues())
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Timeout(); got != 9*time.Second {
		t.Fatalf("Timeout after flag removal = %v, want 9s (the save must outlive the flag)", got)
	}
}

// Setting a flag-backed field TO the shipped default must also persist: left
// unpersisted, the running value would revert to the flag's value on the next
// boot while the flag remains, silently undoing the save.
func TestUpdatePersistsShippedValueOverFlag(t *testing.T) {
	ctx := context.Background()
	def := shippedValues()
	def.Timeout = 9 * time.Second // flag-backed
	c, st := openBaseline(t, def)

	if _, err := c.Update(ctx, Patch{Timeout: pv(3 * time.Second)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	kv, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if kv[keyTimeout] != "3" {
		t.Fatalf("timeout_s = %q, want \"3\" (a change back to the shipped default must outlive the flag)", kv[keyTimeout])
	}
	// A restart with the flag still present keeps the saved value.
	c2, err := New(ctx, st, def)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Timeout(); got != 3*time.Second {
		t.Fatalf("Timeout on restart with the flag = %v, want the saved 3s", got)
	}
}

// A submitted field that matches the shipped default and has no stored key is
// skipped: the store stays a sparse overlay, so a future shipped-default change
// still flows through, and an untouched full-form save can't make a fresh
// install look established.
func TestUpdateSkipsUntouchedShippedDefaults(t *testing.T) {
	ctx := context.Background()
	c, st := openBaseline(t, shippedValues())
	before, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A full-form-style save echoing the shipped defaults.
	if _, err := c.Update(ctx, Patch{
		Latency: pv(5 * time.Second), LatencyEnabled: pv(true), Speed: pv(time.Hour),
		Timeout: pv(3 * time.Second), DownAfter: pv(2), UpAfter: pv(1),
		SpeedtestEnabled: pv(false), SpeedtestOnReconnect: pv(true), IPv6Mode: pv("auto"),
		Retention: pv(30 * 24 * time.Hour), SpeedRetention: pv(365 * 24 * time.Hour),
		DowntimeRetention: pv(365 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(before, after) {
		t.Errorf("an untouched default-matching save must persist nothing:\n before %v\n after  %v", before, after)
	}
	if hasPriorConfiguration(after) {
		t.Error("an untouched default-matching save must not make a fresh install look established")
	}
}

// A normal change to an already-stored key persists; a no-op save of the same
// stored value writes nothing.
func TestUpdateStoredValueChangeAndNoop(t *testing.T) {
	ctx := context.Background()
	c, st := openBaseline(t, shippedValues())

	if _, err := c.Update(ctx, Patch{Timeout: pv(9 * time.Second)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	snap1, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap1[keyTimeout] != "9" {
		t.Fatalf("timeout_s = %q, want \"9\"", snap1[keyTimeout])
	}

	// No-op save of the stored value: the table must be byte-identical.
	if _, err := c.Update(ctx, Patch{Timeout: pv(9 * time.Second)}); err != nil {
		t.Fatalf("no-op Update: %v", err)
	}
	snap2, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(snap1, snap2) {
		t.Errorf("a no-op save of a stored value must write nothing:\n before %v\n after  %v", snap1, snap2)
	}

	// A real change to the stored value persists.
	if _, err := c.Update(ctx, Patch{Timeout: pv(5 * time.Second)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	snap3, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap3[keyTimeout] != "5" {
		t.Fatalf("timeout_s = %q, want \"5\" (a stored-value change must persist)", snap3[keyTimeout])
	}
}

// A flag-backed field the save does NOT submit stays unpersisted: only an
// explicit save may pin a flag value, so an unrelated save never freezes it.
func TestUpdateLeavesUnsubmittedFlagValuesUnpersisted(t *testing.T) {
	ctx := context.Background()
	def := shippedValues()
	def.Timeout = 9 * time.Second // flag-backed, never submitted below
	c, st := openBaseline(t, def)

	if _, err := c.Update(ctx, Patch{DownAfter: pv(5)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	kv, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if kv[keyDownAfter] != "5" {
		t.Fatalf("down_after = %q, want \"5\"", kv[keyDownAfter])
	}
	if _, frozen := kv[keyTimeout]; frozen {
		t.Errorf("timeout_s was persisted by a save that did not submit it (froze the flag value %q)", kv[keyTimeout])
	}
}

// Patch.keys must mirror apply: every Patch field marks exactly the persisted
// key its same-named Values field feeds in formKeys. A field missing from keys
// would quietly lose the flag-backed-save fix for that one setting.
func TestPatchKeysCoversEveryField(t *testing.T) {
	pt := reflect.TypeOf(Patch{})
	base := formKeys(Values{})
	seen := map[string]string{} // key -> Patch field, to catch double-mapping
	for i := 0; i < pt.NumField(); i++ {
		sf := pt.Field(i)
		var p Patch
		pf := reflect.ValueOf(&p).Elem().Field(i)
		switch sf.Type.Kind() {
		case reflect.Ptr:
			pf.Set(reflect.New(sf.Type.Elem()))
		case reflect.Slice:
			pf.Set(reflect.MakeSlice(sf.Type, 0, 0))
		default:
			t.Fatalf("Patch.%s is a %s; extend this test so the new field kind is covered", sf.Name, sf.Type)
		}
		ks := p.keys()
		if len(ks) != 1 {
			t.Errorf("Patch.%s: keys() marked %d keys, want exactly 1", sf.Name, len(ks))
			continue
		}
		var k string
		for k2 := range ks {
			k = k2
		}
		if prev, dup := seen[k]; dup {
			t.Errorf("Patch.%s and Patch.%s both map to key %q", sf.Name, prev, k)
		}
		seen[k] = sf.Name
		// The marked key must be the one formKeys changes when the same-named
		// Values field changes - that pins the field->key mapping itself.
		var v Values
		f := reflect.ValueOf(&v).Elem().FieldByName(sf.Name)
		if !f.IsValid() || !sentinel(f) {
			t.Fatalf("Values.%s: no sentinel; extend sentinel() in formkeys_drift_test.go", sf.Name)
		}
		mod := formKeys(v)
		changed := ""
		for mk, mval := range mod {
			if base[mk] != mval {
				if changed != "" {
					t.Fatalf("Values.%s changes multiple formKeys entries (%q and %q); this test assumes 1:1", sf.Name, changed, mk)
				}
				changed = mk
			}
		}
		if changed != k {
			t.Errorf("Patch.%s: keys() marks %q but formKeys ties the field to %q", sf.Name, k, changed)
		}
	}
}

// shippedDefaults' constants mirror config.Default() (the flag defaults ARE the
// shipped defaults); this pins them so the two layers can't drift. Every field
// that is not flag-seedable must pass through untouched - those shipped values
// live in the seed itself and no flag can alter them.
func TestShippedDefaultsMatchConfig(t *testing.T) {
	def := config.Default()
	ship := shippedDefaults(Values{})
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Latency", ship.Latency, def.Interval},
		{"LatencyEnabled", ship.LatencyEnabled, def.LatencyEnabled},
		{"Speed", ship.Speed, def.SpeedtestInterval},
		{"Timeout", ship.Timeout, def.Timeout},
		{"DownAfter", ship.DownAfter, def.DownAfter},
		{"UpAfter", ship.UpAfter, def.UpAfter},
		{"SpeedtestEnabled", ship.SpeedtestEnabled, def.SpeedtestEnabled},
		{"SpeedtestOnReconnect", ship.SpeedtestOnReconnect, def.SpeedtestOnReconnect},
		{"IPv6Mode", ship.IPv6Mode, def.IPv6Mode},
		{"Retention", ship.Retention, def.Retention},
		{"SpeedRetention", ship.SpeedRetention, def.SpeedRetention},
		{"DowntimeRetention", ship.DowntimeRetention, def.DowntimeRetention},
	}
	flagSeeded := map[string]bool{}
	for _, c := range checks {
		flagSeeded[c.name] = true
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("shippedDefaults.%s = %v, want config.Default()'s %v", c.name, c.got, c.want)
		}
	}
	// Pass-through: seed every field with a sentinel; only the flag-seedable
	// fields may change.
	var v Values
	rv := reflect.ValueOf(&v).Elem()
	for i := 0; i < rv.NumField(); i++ {
		if !sentinel(rv.Field(i)) {
			t.Fatalf("Values.%s: no sentinel; extend sentinel() in formkeys_drift_test.go", rv.Type().Field(i).Name)
		}
	}
	out := reflect.ValueOf(shippedDefaults(v))
	for i := 0; i < rv.NumField(); i++ {
		name := rv.Type().Field(i).Name
		if flagSeeded[name] {
			continue
		}
		if !reflect.DeepEqual(rv.Field(i).Interface(), out.Field(i).Interface()) {
			t.Errorf("shippedDefaults reset non-flag field Values.%s from %v to %v; it must pass through",
				name, rv.Field(i).Interface(), out.Field(i).Interface())
		}
	}
}
