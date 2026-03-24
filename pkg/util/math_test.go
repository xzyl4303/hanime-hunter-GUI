package util_test

import (
	"testing"

	"github.com/acgtools/hanime-hunter/pkg/util"
	"github.com/magiconair/properties/assert"
)

func TestFormatSize(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		size int64
		res  string
	}{
		{
			name: "0",
			size: 0,
			res:  "0.00 B/s",
		},
		{
			name: "512",
			size: 512,
			res:  "512.00 B/s",
		},
		{
			name: "1024",
			size: 1024,
			res:  "1.00 KiB/s",
		},
		{
			name: "1048576",
			size: 1048576,
			res:  "1.00 MiB/s",
		},
		{
			name: "1073741824",
			size: 1073741824,
			res:  "1.00 GiB/s",
		},
		{
			name: "1023",
			size: 1023,
			res:  "1023.00 B/s",
		},
		{
			name: "1025",
			size: 1025,
			res:  "1.00 KiB/s",
		},
		{
			name: "1,099,511,627,776", // 1TiB
			size: 1099511627776,
			res:  "1024.00 GiB/s",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := util.FormatSize(tc.size)

			assert.Equal(t, s, tc.res)
		})
	}
}
