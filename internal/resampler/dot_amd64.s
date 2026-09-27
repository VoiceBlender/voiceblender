//go:build !purego

#include "textflag.h"

// func dotF64AVX2(a, b []float64) float64
TEXT ·dotF64AVX2(SB), NOSPLIT, $0-56
	MOVQ   a_base+0(FP), SI
	MOVQ   a_len+8(FP), CX
	MOVQ   b_base+24(FP), DI
	VXORPD Y0, Y0, Y0
	VXORPD Y1, Y1, Y1
	VXORPD Y2, Y2, Y2
	VXORPD Y3, Y3, Y3

loop16:
	CMPQ        CX, $16
	JLT         loop4
	VMOVUPD     (SI), Y4
	VMOVUPD     32(SI), Y5
	VMOVUPD     64(SI), Y6
	VMOVUPD     96(SI), Y7
	VFMADD231PD (DI), Y4, Y0
	VFMADD231PD 32(DI), Y5, Y1
	VFMADD231PD 64(DI), Y6, Y2
	VFMADD231PD 96(DI), Y7, Y3
	ADDQ        $128, SI
	ADDQ        $128, DI
	SUBQ        $16, CX
	JMP         loop16

loop4:
	CMPQ        CX, $4
	JLT         reduce
	VMOVUPD     (SI), Y4
	VFMADD231PD (DI), Y4, Y0
	ADDQ        $32, SI
	ADDQ        $32, DI
	SUBQ        $4, CX
	JMP         loop4

reduce:
	VADDPD       Y1, Y0, Y0
	VADDPD       Y3, Y2, Y2
	VADDPD       Y2, Y0, Y0
	VEXTRACTF128 $1, Y0, X1
	VADDPD       X1, X0, X0
	VPERMILPD    $1, X0, X1
	VADDSD       X1, X0, X0
	VZEROUPPER
	MOVSD        X0, ret+48(FP)
	RET
