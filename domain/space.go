package domain

import (
	"crypto/rand"
	"encoding/base64"
)

const root = "@"
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
const idLength = 20

var base = base64.NewEncoding(alphabet).WithPadding(base64.NoPadding)

func Root() string {
	return root
}

func BaseLength() int {
	return len(alphabet)
}

func IdLength() int {
	return idLength
}

func Parent(hash string) string {
	if hash == root {
		return ""
	}
	if l := len(hash) - 1; l == 0 {
		return root
	} else {
		return hash[:l]
	}
}

func RandomId() []byte {
	id := make([]byte, idLength)
	_, err := rand.Read(id)
	if err != nil {
		panic(err)
	}
	return id
}

func EncodeId(id []byte) string {
	encoded := make([]byte, base.EncodedLen(idLength))
	base.Encode(encoded, id)
	return string(encoded)
}

func DecodeName(name string) ([]byte, error) {
	id, err := base.DecodeString(name)
	if err != nil {
		return nil, err
	}
	return id, nil
}

func DecodeLocation(universe, location string) ([]byte, error) {
	key := universe
	if len(key) == 0 {
		key = location
	}
	if location != root {
		key = location + key[len(location):]
	}
	return DecodeName(key)
}
