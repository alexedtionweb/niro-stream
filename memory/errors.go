package memory

import "github.com/alexedtionweb/niro-stream"

var errNilStore = niro.NewError(niro.ErrCodeInvalidRequest, "memory store is nil")
