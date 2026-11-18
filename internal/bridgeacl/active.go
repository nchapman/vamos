package bridgeacl

import "github.com/nchapman/vamos/internal/agentproto"

// KeyedPrincipals returns only principals with a current restricted key. This
// matters for an already-open bridge between the two durable revoke writes.
func KeyedPrincipals(principalPath, keyPath string) (map[string]agentproto.Principal, error) {
	var err error
	for range 2 {
		principals, readErr := ReadPrincipals(principalPath)
		if readErr != nil {
			return nil, readErr
		}
		_, keys, readErr := readKeys(keyPath, principals)
		if readErr != nil {
			// A grant may have published both new files after we read the old ACL.
			err = readErr
			continue
		}
		active := make(map[string]agentproto.Principal, len(keys))
		for _, k := range keys {
			active[k.id] = principals[k.id]
		}
		return active, nil
	}
	return nil, err
}
