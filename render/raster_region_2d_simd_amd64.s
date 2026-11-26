//go:build amd64 && !purego && !raster2dscalar

#include "go_asm.h"
#include "textflag.h"

// 2^24 per uint64 lane for complementary horizontal weights.
DATA unit<>+0(SB)/8, $16777216
DATA unit<>+8(SB)/8, $16777216
GLOBL unit<>(SB), RODATA|NOPTR, $16
DATA mask14<>+0(SB)/8, $16383
DATA mask14<>+8(SB)/8, $16383
GLOBL mask14<>(SB), RODATA|NOPTR, $16
// The inclusive canonical fallback covers all three adjacent bins around
// the half-integer. Use impossible high dwords so flags occupy the low words.
DATA tieLow<>+0(SB)/8, $0xffffffff00001fff
DATA tieLow<>+8(SB)/8, $0xffffffff00001fff
GLOBL tieLow<>(SB), RODATA|NOPTR, $16
DATA tieMid<>+0(SB)/8, $0xffffffff00002000
DATA tieMid<>+8(SB)/8, $0xffffffff00002000
GLOBL tieMid<>(SB), RODATA|NOPTR, $16
DATA tieHigh<>+0(SB)/8, $0xffffffff00002001
DATA tieHigh<>+8(SB)/8, $0xffffffff00002001
GLOBL tieHigh<>(SB), RODATA|NOPTR, $16
DATA half<>+0(SB)/8, $140737488355328
DATA half<>+8(SB)/8, $140737488355328
GLOBL half<>(SB), RODATA|NOPTR, $16
DATA grayRGB<>+0(SB)/8, $0x010101
DATA grayRGB<>+8(SB)/8, $0x010101
GLOBL grayRGB<>(SB), RODATA|NOPTR, $16
DATA opaque<>+0(SB)/8, $0xff000000
DATA opaque<>+8(SB)/8, $0xff000000
GLOBL opaque<>(SB), RODATA|NOPTR, $16

// rasterGraySIMDRow takes a covered span of pairs and an exact Q24 vertical
// row. All lo/hi indices stay within the retained scratch row. Each approximate
// result is left*(2^24−weight)+right*weight in Q48, the same integer as
// left<<24+(right−left)*weight. Both products fit unsigned 56 bits.
TEXT ·rasterGraySIMDRow(SB), NOSPLIT, $0-33
	MOVQ dst+0(FP), DI
	MOVQ src+8(FP), BX
	MOVQ pairs+16(FP), SI
	MOVQ count+24(FP), R8

	MOVOU unit<>(SB), X8
	MOVOU mask14<>(SB), X9
	MOVOU tieLow<>(SB), X10
	MOVOU tieMid<>(SB), X11
	MOVOU tieHigh<>(SB), X12
	MOVOU half<>(SB), X13
	MOVOU grayRGB<>(SB), X14
	MOVOU opaque<>(SB), X15
	PXOR X7, X7

pair:
	MOVL rasterGrayColumnPair_lo(SI), AX
	MOVL rasterGrayColumnPair_lo+4(SI), CX
	MOVQ (BX)(AX*8), X0
	MOVHPS (BX)(CX*8), X0

	MOVL rasterGrayColumnPair_hi(SI), AX
	MOVL rasterGrayColumnPair_hi+4(SI), CX
	MOVQ (BX)(AX*8), X1
	MOVHPS (BX)(CX*8), X1

	MOVOU rasterGrayColumnPair_weight(SI), X2
	MOVOU X8, X3
	PSUBQ X2, X3
	PMULULQ X3, X0
	PMULULQ X2, X1
	PADDQ X1, X0

	// Discard integer bits and reduce the fractional part to bins of
	// 2^-14. An alpha of zero marks a scalar canonical retry.
	MOVOU X0, X1
	PSRLQ $34, X1
	PAND X9, X1
	MOVOU X1, X2
	MOVOU X1, X3
	PCMPEQL X10, X1
	PCMPEQL X11, X2
	PCMPEQL X12, X3
	POR X2, X1
	POR X3, X1
	POR X1, X7

	PADDQ X13, X0
	PSRLQ $48, X0
	PMULULQ X14, X0
	POR X15, X0
	PANDN X0, X1
	PSHUFL $0x88, X1, X1
	MOVQ X1, 0(DI)

	ADDQ $rasterGrayColumnPair__size, SI
	ADDQ $8, DI
	DECQ R8
	JNZ pair

	PMOVMSKB X7, AX
	TESTQ AX, AX
	SETNE hasTies+32(FP)
	RET
