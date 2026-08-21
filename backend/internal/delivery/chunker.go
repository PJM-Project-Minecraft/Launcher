package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

const (
	chunkMin = 1 << 20
	chunkAvg = 4 << 20
	chunkMax = 8 << 20
)

type chunkData struct {
	Hash string
	Data []byte
}

// splitChunks implements the FastCDC gear-hash shape with fixed project
// parameters. The chunker is streaming and deterministic; changing these
// constants is a wire-format change and requires a schema bump.
func splitChunks(reader io.Reader, emit func(chunkData) error) error {
	buffer := make([]byte, 0, chunkMax)
	input := make([]byte, 256<<10)
	var gear uint64
	for {
		read, err := reader.Read(input)
		for _, value := range input[:read] {
			buffer = append(buffer, value)
			gear = (gear << 1) + gearTable[value]

			length := len(buffer)
			boundary := length >= chunkMax
			if length >= chunkMin && !boundary {
				mask := uint64((1 << 23) - 1)
				if length >= chunkAvg {
					mask = (1 << 21) - 1
				}
				boundary = gear&mask == 0
			}
			if boundary {
				if emitErr := emitChunk(buffer, emit); emitErr != nil {
					return emitErr
				}
				// Ownership of the emitted slice belongs to the callback.
				buffer = make([]byte, 0, chunkMax)
				gear = 0
			}
		}
		if err != nil && err != io.EOF {
			return err
		}
		if err == io.EOF {
			if len(buffer) > 0 {
				return emitChunk(buffer, emit)
			}
			return nil
		}
	}
}

func emitChunk(data []byte, emit func(chunkData) error) error {
	digest := sha256.Sum256(data)
	return emit(chunkData{Hash: hex.EncodeToString(digest[:]), Data: data})
}

var gearTable = func() [256]uint64 {
	var table [256]uint64
	for index := range table {
		table[index] = gearValue(byte(index))
	}
	return table
}()

func gearValue(value byte) uint64 {
	// SplitMix64 gives every byte a stable pseudo-random gear value without a
	// 256-entry magic table.
	x := uint64(value) + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
