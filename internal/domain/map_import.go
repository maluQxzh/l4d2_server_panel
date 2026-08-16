package domain

const (
	MapPathEncodingAuto = "auto"
	MapPathEncodingUTF8 = "utf8"
	MapPathEncodingGBK  = "gbk"
	MapPathEncodingBig5 = "big5"
)

func ValidMapPathEncoding(value string) bool {
	switch value {
	case MapPathEncodingAuto, MapPathEncodingUTF8, MapPathEncodingGBK, MapPathEncodingBig5:
		return true
	default:
		return false
	}
}
