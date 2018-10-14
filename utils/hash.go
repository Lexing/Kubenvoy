package utils

import (
	"github.com/gogo/protobuf/proto"
	"github.com/minio/highwayhash"
)

// ProtoFingerprint implements fingerprint for proto, it's thread-safe.
func ProtoFingerprint(msg proto.Message) (uint64, error) {
	key := []byte("my hobby bloa as a brother ok ?)")
	data, err := proto.Marshal(msg)
	if err != nil {
		return 0, err
	}

	hhash, _ := highwayhash.New64(key)
	hhash.Reset()
	hhash.Write(data)
	return hhash.Sum64(), nil
}
