package agent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"unicode/utf16"

	"github.com/ulikunitz/xz/lzma"
)

const sevenZipDictionarySize = 8 << 20

type sevenZipEntry struct {
	name             string
	packedSize       uint64
	uncompressedSize uint64
	crc32            uint32
}

type sevenZipWriter struct {
	file    *os.File
	entries []sevenZipEntry
	closed  bool
}

func newSevenZipWriter(file *os.File) (*sevenZipWriter, error) {
	if file == nil {
		return nil, errors.New("7z output file is required")
	}
	if _, err := file.Write(make([]byte, 32)); err != nil {
		return nil, err
	}
	return &sevenZipWriter{file: file}, nil
}

func (w *sevenZipWriter) Add(name string, source io.Reader) (uint64, uint32, error) {
	if w.closed || name == "" || source == nil {
		return 0, 0, errors.New("invalid 7z entry")
	}
	start, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, err
	}
	encoder, err := (lzma.Writer2Config{DictCap: sevenZipDictionarySize}).NewWriter2(w.file)
	if err != nil {
		return 0, 0, err
	}
	checksum := crc32.NewIEEE()
	size, copyErr := io.Copy(encoder, io.TeeReader(source, checksum))
	closeErr := encoder.Close()
	if copyErr != nil {
		return 0, 0, copyErr
	}
	if closeErr != nil {
		return 0, 0, closeErr
	}
	end, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, err
	}
	w.entries = append(w.entries, sevenZipEntry{name: name, packedSize: uint64(end - start),
		uncompressedSize: uint64(size), crc32: checksum.Sum32()})
	return uint64(size), checksum.Sum32(), nil
}

func (w *sevenZipWriter) Close() error {
	if w.closed {
		return errors.New("7z writer is already closed")
	}
	w.closed = true
	if len(w.entries) == 0 {
		return errors.New("7z archive must contain at least one file")
	}
	header, err := w.header()
	if err != nil {
		return err
	}
	headerStart, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := w.file.Write(header); err != nil {
		return err
	}
	startHeader := make([]byte, 20)
	binary.LittleEndian.PutUint64(startHeader[0:8], uint64(headerStart-32))
	binary.LittleEndian.PutUint64(startHeader[8:16], uint64(len(header)))
	binary.LittleEndian.PutUint32(startHeader[16:20], crc32.ChecksumIEEE(header))
	signatureHeader := make([]byte, 32)
	copy(signatureHeader[0:6], []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c})
	signatureHeader[6], signatureHeader[7] = 0, 4
	binary.LittleEndian.PutUint32(signatureHeader[8:12], crc32.ChecksumIEEE(startHeader))
	copy(signatureHeader[12:], startHeader)
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := w.file.Write(signatureHeader); err != nil {
		return err
	}
	_, err = w.file.Seek(0, io.SeekEnd)
	return err
}

func (w *sevenZipWriter) header() ([]byte, error) {
	const (
		idEnd             = 0
		idHeader          = 1
		idMainStreamsInfo = 4
		idFilesInfo       = 5
		idPackInfo        = 6
		idUnpackInfo      = 7
		idSubStreamsInfo  = 8
		idSize            = 9
		idCRC             = 10
		idFolder          = 11
		idCodersUnpack    = 12
		idName            = 17
	)
	var header bytes.Buffer
	header.WriteByte(idHeader)
	header.WriteByte(idMainStreamsInfo)
	header.WriteByte(idPackInfo)
	writeSevenZipUint64(&header, 0)
	writeSevenZipUint64(&header, uint64(len(w.entries)))
	header.WriteByte(idSize)
	for _, entry := range w.entries {
		writeSevenZipUint64(&header, entry.packedSize)
	}
	header.WriteByte(idEnd)
	header.WriteByte(idUnpackInfo)
	header.WriteByte(idFolder)
	writeSevenZipUint64(&header, uint64(len(w.entries)))
	header.WriteByte(0)
	for range w.entries {
		writeSevenZipUint64(&header, 1)
		header.WriteByte(0x21)
		header.WriteByte(0x21)
		writeSevenZipUint64(&header, 1)
		header.WriteByte(lzma.EncodeDictCap(sevenZipDictionarySize))
	}
	header.WriteByte(idCodersUnpack)
	for _, entry := range w.entries {
		writeSevenZipUint64(&header, entry.uncompressedSize)
	}
	header.WriteByte(idCRC)
	header.WriteByte(1)
	for _, entry := range w.entries {
		if err := binary.Write(&header, binary.LittleEndian, entry.crc32); err != nil {
			return nil, err
		}
	}
	header.WriteByte(idEnd)
	header.WriteByte(idSubStreamsInfo)
	header.WriteByte(idEnd)
	header.WriteByte(idEnd)
	header.WriteByte(idFilesInfo)
	writeSevenZipUint64(&header, uint64(len(w.entries)))
	names := sevenZipNames(w.entries)
	header.WriteByte(idName)
	writeSevenZipUint64(&header, uint64(len(names)+1))
	header.WriteByte(0)
	header.Write(names)
	header.WriteByte(idEnd)
	header.WriteByte(idEnd)
	return header.Bytes(), nil
}

func sevenZipNames(entries []sevenZipEntry) []byte {
	var names bytes.Buffer
	for _, entry := range entries {
		for _, unit := range utf16.Encode([]rune(entry.name)) {
			_ = binary.Write(&names, binary.LittleEndian, unit)
		}
		_ = binary.Write(&names, binary.LittleEndian, uint16(0))
	}
	return names.Bytes()
}

func writeSevenZipUint64(destination *bytes.Buffer, value uint64) {
	for length := 0; length < 8; length++ {
		limit := uint64(1) << (7 * (length + 1))
		if value < limit {
			prefix := byte(0)
			if length > 0 {
				prefix = byte(0xff << (8 - length))
			}
			destination.WriteByte(prefix | byte(value>>(8*length)))
			for index := 0; index < length; index++ {
				destination.WriteByte(byte(value >> (8 * index)))
			}
			return
		}
	}
	destination.WriteByte(0xff)
	for index := 0; index < 8; index++ {
		destination.WriteByte(byte(value >> (8 * index)))
	}
}
