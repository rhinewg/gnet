module github.com/panjf2000/gnet

go 1.16

require (
	github.com/klauspost/reedsolomon v1.10.0
	github.com/panjf2000/ants/v2 v2.4.7
	github.com/pkg/errors v0.8.1
	github.com/stretchr/testify v1.7.0
	github.com/templexxx/xorsimd v0.4.1
	github.com/tjfoc/gmsm v1.4.1
	github.com/valyala/bytebufferpool v1.0.0
	go.uber.org/atomic v1.9.0 // indirect
	go.uber.org/multierr v1.7.0 // indirect
	go.uber.org/zap v1.19.1
	golang.org/x/crypto v0.0.0-20201012173705-84dcc777aaee
	golang.org/x/net v0.0.0-20210405180319-a5a99cb37ef4
	golang.org/x/sys v0.0.0-20211204120058-94396e421777
	gopkg.in/natefinch/lumberjack.v2 v2.0.0
)

// Go module checksum mismatch, see https://github.com/panjf2000/gnet/issues/219
// retract v1.4.5
