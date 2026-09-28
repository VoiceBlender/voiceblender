//go:build !purego

#include "textflag.h"

// func dotF64NEON(a, b []float64) float64
TEXT ·dotF64NEON(SB), NOSPLIT, $0-56
	MOVD a_base+0(FP), R0
	MOVD a_len+8(FP), R2
	MOVD b_base+24(FP), R1
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16

loop8:
	CMP    $8, R2
	BLT    reduce
	VLD1.P 64(R0), [V4.D2, V5.D2, V6.D2, V7.D2]
	VLD1.P 64(R1), [V16.D2, V17.D2, V18.D2, V19.D2]
	VFMLA  V4.D2, V16.D2, V0.D2
	VFMLA  V5.D2, V17.D2, V1.D2
	VFMLA  V6.D2, V18.D2, V2.D2
	VFMLA  V7.D2, V19.D2, V3.D2
	SUB    $8, R2
	B      loop8

reduce:
	// The Go assembler has no vector FADD mnemonic.
	WORD  $0x4E61D400 // FADD V0.2D, V0.2D, V1.2D
	WORD  $0x4E63D442 // FADD V2.2D, V2.2D, V3.2D
	WORD  $0x4E62D400 // FADD V0.2D, V0.2D, V2.2D
	VMOV  V0.D[1], R3
	FMOVD R3, F1
	FADDD F1, F0, F0
	FMOVD F0, ret+48(FP)
	RET
