package addr

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Address
		wantErr bool
	}{
		{in: "manager-4123", want: Address{Machine: "local", Agent: "manager-4123"}},
		{in: "local/manager-4123", want: Address{Machine: "local", Agent: "manager-4123"}},
		{in: "recado://local/manager-4123", want: Address{Machine: "local", Agent: "manager-4123"}},
		{in: "rpi/sub.1_x", want: Address{Machine: "rpi", Agent: "sub.1_x"}},
		{in: "recado://rpi/sub-1", want: Address{Machine: "rpi", Agent: "sub-1"}},
		{
			in:   "recado+upstream://host.example/manager-1",
			want: Address{Machine: "host.example", Agent: "manager-1", Upstream: true},
		},

		{in: "", wantErr: true},
		{in: "recado://local", wantErr: true},
		{in: "recado://", wantErr: true},
		{in: "recado:///sub-1", wantErr: true},
		{in: "recado://local/sub-1/x", wantErr: true},
		{in: "a/b/c", wantErr: true},
		{in: "/sub-1", wantErr: true},
		{in: "local/", wantErr: true},
		{in: "Manager-1", wantErr: true},
		{in: "-sub-1", wantErr: true},
		{in: "sub 1", wantErr: true},
		{in: "rpi/sub-1/", wantErr: true},
		{in: "http://local/sub-1", wantErr: true},
		{in: "recado+upstream://host/sub-1", want: Address{Machine: "host", Agent: "sub-1", Upstream: true}},
		{in: "host/sub-1", want: Address{Machine: "host", Agent: "sub-1"}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestFormat(t *testing.T) {
	local := Local("sub-1")
	if got := local.String(); got != "recado://local/sub-1" {
		t.Errorf("local.String() = %q", got)
	}
	if got := local.Display(); got != "sub-1" {
		t.Errorf("local.Display() = %q", got)
	}

	remote, err := Parse("recado://rpi/sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := remote.String(); got != "recado://rpi/sub-1" {
		t.Errorf("remote.String() = %q", got)
	}
	if got := remote.Display(); got != "rpi/sub-1" {
		t.Errorf("remote.Display() = %q", got)
	}

	up, err := Parse("recado+upstream://web/manager-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := up.String(); got != "recado+upstream://web/manager-1" {
		t.Errorf("up.String() = %q", got)
	}
	if up.IsLocal() {
		t.Error("upstream address reported local")
	}
}
