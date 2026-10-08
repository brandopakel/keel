#include "textflag.h"

// func ticks() uint64
TEXT ·ticks(SB),NOSPLIT,$0-8
	RDTSC
	SHLQ	$32, DX
	ORQ	DX, AX
	MOVQ	AX, ret+0(FP)
	RET
