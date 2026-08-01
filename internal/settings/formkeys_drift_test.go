package settings

import (
	"reflect"
	"testing"
	"time"
)

// sentinel writes a distinctive non-zero value into one Values field, chosen so
// that a field which fails to round-trip lands back on its zero and the compare
// notices. Reports false for a kind it doesn't know, so a future field type
// fails loudly here instead of being skipped in silence.
func sentinel(f reflect.Value) bool {
	if f.Type() == reflect.TypeOf(time.Duration(0)) {
		f.Set(reflect.ValueOf(7 * time.Second)) // whole seconds: sec()/secs() is the wire format
		return true
	}
	switch f.Type() {
	case reflect.TypeOf([]IperfTarget(nil)):
		f.Set(reflect.ValueOf([]IperfTarget{{Label: "NAS", Addr: "10.0.0.5:5201"}}))
		return true
	case reflect.TypeOf([]Window(nil)):
		f.Set(reflect.ValueOf([]Window{{Days: AllDays, Start: 540, End: 1020}}))
		return true
	}
	switch f.Kind() {
	case reflect.Bool:
		f.SetBool(true)
		return true
	case reflect.String:
		f.SetString("both") // a value every string setting here accepts verbatim
		return true
	case reflect.Int:
		f.SetInt(7)
		return true
	case reflect.Float64:
		f.SetFloat(3.5)
		return true
	}
	return false
}

// THE DRIFT GUARD the settings layer was missing. Adding one user-visible
// setting means editing ~10 places, and exactly one of those omissions is
// invisible: leave the field out of formKeys and it applies in memory, echoes
// back correctly from the POST, and is GONE at the next restart.
//
// TestFormKeysOverlayRoundTrip cannot catch it. It compares a hand-written
// struct literal against itself, so a field nobody remembered to add to that
// literal is zero on both sides and passes - the test agrees with the bug.
//
// This one is exhaustive by construction: it walks Patch (which IS the set of
// form-owned settings - a field with no Patch entry cannot be sent by the form
// at all), sets each matching Values field to a non-zero sentinel one at a time,
// and requires formKeys->overlay to bring it back. Miss the formKeys entry or
// the overlay case and the field returns as its zero value, naming itself.
func TestFormKeysCoversEveryPatchField(t *testing.T) {
	pt, vt := reflect.TypeOf(Patch{}), reflect.TypeOf(Values{})
	for i := 0; i < pt.NumField(); i++ {
		name := pt.Field(i).Name
		if _, ok := vt.FieldByName(name); !ok {
			t.Errorf("Patch.%s has no Values field of the same name; apply() cannot be mapping it", name)
			continue
		}
		var v Values
		f := reflect.ValueOf(&v).Elem().FieldByName(name)
		if !sentinel(f) {
			t.Fatalf("Values.%s is a %s, which this test has no sentinel for - extend sentinel() "+
				"so the new field is actually covered rather than silently skipped", name, f.Type())
		}
		want := f.Interface()
		got := reflect.ValueOf(overlay(Values{}, formKeys(v))).FieldByName(name).Interface()
		if !reflect.DeepEqual(want, got) {
			t.Errorf("Values.%s did not survive formKeys->overlay: set %v, got back %v. "+
				"A form setting missing from formKeys saves in memory, echoes back from the POST, "+
				"and is gone after a restart; one missing from overlay is never read back at all.",
				name, want, got)
		}
	}
}
