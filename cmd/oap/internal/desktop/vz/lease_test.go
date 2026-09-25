package vz

import "testing"

func TestLeaseIPForMAC(t *testing.T) {
	const singleBlock = `{
	name=
	ip_address=192.168.64.2
	hw_address=1,6e:14:dc:de:1e:0
	identifier=1,6e:14:dc:de:1e:0
	lease=0x686000000
}
`

	const multiBlock = `{
	name=
	ip_address=192.168.64.5
	hw_address=1,aa:bb:cc:dd:ee:ff
	identifier=1,aa:bb:cc:dd:ee:ff
	lease=0x686000000
}
{
	name=
	ip_address=192.168.64.2
	hw_address=1,6e:14:dc:de:1e:0
	identifier=1,6e:14:dc:de:1e:0
	lease=0x686000000
}
`

	// hw_address whose octets need leading-zero normalization the other
	// direction: "06" written in full vs. the trimmed "6" the query side
	// might use.
	const leadingZeroBlock = `{
	name=
	ip_address=192.168.64.9
	hw_address=1,02:04:06:08:0a:0c
}
`

	cases := []struct {
		name      string
		contents  string
		queryMAC  string
		wantIP    string
		wantFound bool
	}{
		{
			name:      "single block: exact-form MAC matches trimmed-octet lease entry",
			contents:  singleBlock,
			queryMAC:  "6e:14:dc:de:1e:00",
			wantIP:    "192.168.64.2",
			wantFound: true,
		},
		{
			name:      "single block: MAC not present returns not-found",
			contents:  singleBlock,
			queryMAC:  "00:11:22:33:44:55",
			wantIP:    "",
			wantFound: false,
		},
		{
			name:      "multiple blocks: matches the second block, not the first",
			contents:  multiBlock,
			queryMAC:  "6e:14:dc:de:1e:00",
			wantIP:    "192.168.64.2",
			wantFound: true,
		},
		{
			name:      "multiple blocks: matches the first block",
			contents:  multiBlock,
			queryMAC:  "aa:bb:cc:dd:ee:ff",
			wantIP:    "192.168.64.5",
			wantFound: true,
		},
		{
			name:      "leading-zero octet in lease (06) matches full-form query octet",
			contents:  leadingZeroBlock,
			queryMAC:  "02:04:06:08:0a:0c",
			wantIP:    "192.168.64.9",
			wantFound: true,
		},
		{
			name:      "leading-zero octet in lease (06) matches trimmed query octet (6)",
			contents:  leadingZeroBlock,
			queryMAC:  "2:4:6:8:a:c",
			wantIP:    "192.168.64.9",
			wantFound: true,
		},
		{
			name:      "empty file returns not-found",
			contents:  "",
			queryMAC:  "6e:14:dc:de:1e:00",
			wantIP:    "",
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotIP, gotFound := leaseIPForMAC(tc.contents, tc.queryMAC)
			if gotFound != tc.wantFound {
				t.Fatalf("leaseIPForMAC() found = %v, want %v", gotFound, tc.wantFound)
			}
			if gotIP != tc.wantIP {
				t.Fatalf("leaseIPForMAC() ip = %q, want %q", gotIP, tc.wantIP)
			}
		})
	}
}

func TestNormalizeMAC(t *testing.T) {
	want := [6]byte{0x6e, 0x14, 0xdc, 0xde, 0x1e, 0x00}

	cases := []struct {
		name   string
		mac    string
		want   [6]byte
		wantOK bool
	}{
		{name: "full form, no prefix", mac: "6e:14:dc:de:1e:00", want: want, wantOK: true},
		{name: "hw-type prefix + trimmed last octet", mac: "1,6e:14:dc:de:1e:0", want: want, wantOK: true},
		{name: "hw-type prefix, all full octets", mac: "1,6e:14:dc:de:1e:00", want: want, wantOK: true},
		{name: "too few octets is invalid", mac: "6e:14:dc:de:1e", wantOK: false},
		{name: "non-hex octet is invalid", mac: "6e:14:dc:de:1e:zz", wantOK: false},
		{name: "empty string is invalid", mac: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeMAC(tc.mac)
			if ok != tc.wantOK {
				t.Fatalf("normalizeMAC(%q) ok = %v, want %v", tc.mac, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("normalizeMAC(%q) = %v, want %v", tc.mac, got, tc.want)
			}
		})
	}
}
