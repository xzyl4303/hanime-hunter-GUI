package util

import "fmt"

// FormatSize formats the file size and ensure the number of size always > 1.
// the unit of parameter `size` is Byte.
func FormatSize(size int64) string {
	units := [4]string{"B", "KiB", "MiB", "GiB"}

	i := 0
	for v := size; ; i++ {
		if v>>10 < 1 || i > 3 {
			break
		}
		v >>= 10
	}
	if i > 3 { //nolint:gomnd
		i = 3
	}

	tmp := 1 << (i * 10) //nolint:gomnd
	num := float64(size) / float64(tmp)

	return fmt.Sprintf("%.2f %s/s", num, units[i])
}
