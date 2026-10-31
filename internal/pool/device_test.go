package pool

import "testing"

const testKey = "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk="

func TestParseDevices(t *testing.T) {
	ds, err := ParseDevices(`[{"id":"a","private_key":"` + testKey + `","ipv4":"10.64.1.2/32"},{"id":"b","private_key":"` + testKey + `","ipv4":"10.64.1.3"}]`)
	if err != nil || len(ds) != 2 || ds[0].IPv4 != "10.64.1.2" {
		t.Fatalf("%+v %v", ds, err)
	}
	for _, bad := range []string{
		`[{"id":"a","private_key":"nope","ipv4":"10.0.0.1"}]`,
		`[{"id":"a","private_key":"` + testKey + `","ipv4":"fc00::1"}]`,
		`[{"id":"","private_key":"` + testKey + `","ipv4":"10.0.0.1"}]`,
		`[{"id":"a","private_key":"` + testKey + `","ipv4":"10.0.0.1"},{"id":"a","private_key":"` + testKey + `","ipv4":"10.0.0.2"}]`,
		`{`,
	} {
		if _, err := ParseDevices(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if ds, err := ParseDevices(""); err != nil || ds != nil {
		t.Fatal("empty env")
	}
}
