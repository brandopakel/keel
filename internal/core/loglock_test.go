package core

import (
	"bufio"
	"os/exec"
	"testing"
)

// Reverse layout control for part 3, never merged.

// layoutPads keeps every pad linked without calling it.
var layoutPads = []func(){
	layoutPadLock0,
	layoutPadLock1,
	layoutPadLock2,
	layoutPadLock3,
	layoutPadLock4,
	layoutPadLock5,
	layoutPadLock6,
	layoutPadLock7,
	layoutPadLock8,
	layoutPadLock9,
	layoutPadLock10,
	layoutPadLock11,
	layoutPadLock12,
	layoutPadLock13,
	layoutPadLock14,
	layoutPadLock15,
	layoutPadLock16,
	layoutPadLock17,
	layoutPadLock18,
	layoutPadLock19,
	layoutPadLock20,
	layoutPadLock21,
	layoutPadLock22,
	layoutPadLock23,
	layoutPadLock24,
	layoutPadLock25,
	layoutPadLock26,
	layoutPadLock27,
	layoutPadLock28,
	layoutPadLock29,
	layoutPadLock30,
	layoutPadLock31,
	layoutPadLock32,
	layoutPadLock33,
	layoutPadLock34,
	layoutPadLock35,
	layoutPadLock36,
	layoutPadLock37,
	layoutPadLock38,
	layoutPadLock39,
	layoutPadLock40,
	layoutPadLock41,
	layoutPadLock42,
	layoutPadLock43,
	layoutPadLock44,
	layoutPadLock45,
	layoutPadLock46,
	layoutPadLock47,
	layoutPadLock48,
	layoutPadLock49,
	layoutPadLock50,
	layoutPadLock51,
	layoutPadLock52,
	layoutPadLock53,
	layoutPadLock54,
	layoutPadLock55,
	layoutPadLock56,
	layoutPadLock57,
	layoutPadLock58,
	layoutPadLock59,
	layoutPadLock60,
	layoutPadLock61,
	layoutPadLock62,
	layoutPadLock63,
	layoutPadLock64,
	layoutPadOpen0,
	layoutPadOpen1,
	layoutPadOpen2,
	layoutPadOpen3,
	layoutPadOpen4,
	layoutPadClose0,
	layoutPadClose1,
	layoutPadClose2,
	layoutPadTest0,
	layoutPadTest1,
	layoutPadTest2,
	layoutPadTest3,
	layoutPadTest4,
	layoutPadTest5,
	layoutPadTest6,
	layoutPadTest7,
	layoutPadTest8,
	layoutPadTest9,
	layoutPadTest10,
	layoutPadTest11,
	layoutPadTest12,
	layoutPadTest13,
	layoutPadTest14,
	layoutPadTest15,
	layoutPadTest16,
	layoutPadTest17,
	layoutPadTest18,
	layoutPadTest19,
	layoutPadTest20,
	layoutPadTest21,
	layoutPadTest22,
	layoutPadTest23,
	layoutPadTest24,
	layoutPadTest25,
	layoutPadTest26,
	layoutPadTest27,
	layoutPadTest28,
	layoutPadTest29,
	layoutPadTest30,
	layoutPadTest31,
	layoutPadTest32,
	layoutPadTest33,
	layoutPadTest34,
	layoutPadTest35,
	layoutPadTest36,
	layoutPadTest37,
	layoutPadTest38,
	layoutPadTest39,
	layoutPadTest40,
	layoutPadTest41,
	layoutPadTest42,
	layoutPadTest43,
	layoutPadTest44,
	layoutPadTest45,
	layoutPadTest46,
	layoutPadTest47,
	layoutPadTest48,
	layoutPadTest49,
	layoutPadTest50,
	layoutPadTest51,
	layoutPadTest52,
	layoutPadTest53,
	layoutPadTest54,
	layoutPadTest55,
	layoutPadTest56,
	layoutPadTest57,
	layoutPadTest58,
	layoutPadTest59,
	layoutPadTest60,
	layoutPadTest61,
	layoutPadTest62,
	layoutPadTest63,
	layoutPadTest64,
	layoutPadTest65,
	layoutPadTest66,
	layoutPadTest67,
	layoutPadTest68,
	layoutPadTest69,
	layoutPadTest70,
	layoutPadTest71,
	layoutPadTest72,
	layoutPadTest73,
	layoutPadTest74,
	layoutPadTest75,
	layoutPadTest76,
	layoutPadTest77,
	layoutPadTest78,
	layoutPadTest79,
	layoutPadTest80,
	layoutPadTest81,
	layoutPadTest82,
	layoutPadTest83,
	layoutPadTest84,
	layoutPadTest85,
	layoutPadTest86,
	layoutPadTest87,
	layoutPadTest88,
	layoutPadTest89,
	layoutPadTest90,
	layoutPadTest91,
	layoutPadTest92,
	layoutPadTest93,
	layoutPadTest94,
	layoutPadTest95,
	layoutPadTest96,
	layoutPadTest97,
	layoutPadTest98,
	layoutPadTest99,
	layoutPadTest100,
	layoutPadTest101,
	layoutPadTest102,
	layoutPadTest103,
	layoutPadTest104,
	layoutPadTest105,
	layoutPadTest106,
	layoutPadTest107,
	layoutPadTest108,
	layoutPadTest109,
	layoutPadTest110,
	layoutPadTest111,
	layoutPadTest112,
	layoutPadTest113,
	layoutPadTest114,
	layoutPadTest115,
	layoutPadTest116,
	layoutPadTest117,
	layoutPadTest118,
	layoutPadTest119,
	layoutPadTest120,
	layoutPadTest121,
	layoutPadTest122,
	layoutPadTest123,
	layoutPadTest124,
	layoutPadTest125,
	layoutPadTest126,
	layoutPadTest127,
	layoutPadTest128,
	layoutPadTest129,
	layoutPadTest130,
	layoutPadTest131,
	layoutPadTest132,
	layoutPadTest133,
	layoutPadTest134,
	layoutPadTest135,
	layoutPadTest136,
	layoutPadTest137,
	layoutPadTest138,
	layoutPadTest139,
	layoutPadTest140,
	layoutPadTest141,
	layoutPadTest142,
	layoutPadTest143,
	layoutPadTest144,
	layoutPadTest145,
	layoutPadTest146,
	layoutPadTest147,
	layoutPadTest148,
	layoutPadTest149,
	layoutPadTest150,
	layoutPadTest151,
	layoutPadTest152,
	layoutPadTest153,
	layoutPadTest154,
	layoutPadTest155,
	layoutPadTest156,
	layoutPadTest157,
	layoutPadTest158,
	layoutPadTest159,
	layoutPadTest160,
	layoutPadTest161,
	layoutPadTest162,
	layoutPadTest163,
	layoutPadTest164,
	layoutPadTest165,
	layoutPadTest166,
	layoutPadTest167,
	layoutPadTest168,
	layoutPadTest169,
	layoutPadTest170,
	layoutPadTest171,
	layoutPadTest172,
	layoutPadTest173,
	layoutPadTest174,
	layoutPadTest175,
	layoutPadTest176,
	layoutPadTest177,
	layoutPadTest178,
	layoutPadTest179,
	layoutPadTest180,
	layoutPadTest181,
	layoutPadTest182,
	layoutPadTest183,
	layoutPadTest184,
	layoutPadTest185,
	layoutPadTest186,
	layoutPadTest187,
	layoutPadTest188,
	layoutPadTest189,
	layoutPadTest190,
	layoutPadTest191,
	layoutPadTest192,
	layoutPadTest193,
	layoutPadTest194,
	layoutPadTest195,
	layoutPadTest196,
	layoutPadTest197,
	layoutPadTest198,
	layoutPadTest199,
	layoutPadTest200,
	layoutPadTest201,
	layoutPadTest202,
	layoutPadTest203,
	layoutPadTest204,
	layoutPadTest205,
	layoutPadTest206,
	layoutPadTest207,
	layoutPadTest208,
	layoutPadTest209,
	layoutPadTest210,
	layoutPadTest211,
	layoutPadTest212,
	layoutPadTest213,
	layoutPadTest214,
	layoutPadTest215,
	layoutPadTest216,
	layoutPadTest217,
	layoutPadTest218,
	layoutPadTest219,
	layoutPadTest220,
	layoutPadTest221,
	layoutPadTest222,
	layoutPadTest223,
	layoutPadTest224,
	layoutPadTest225,
	layoutPadTest226,
	layoutPadTest227,
	layoutPadTest228,
	layoutPadTest229,
	layoutPadTest230,
	layoutPadTest231,
	layoutPadTest232,
	layoutPadTest233,
	layoutPadTest234,
	layoutPadTest235,
	layoutPadTest236,
	layoutPadTest237,
	layoutPadTest238,
	layoutPadTest239,
	layoutPadTest240,
	layoutPadTest241,
	layoutPadTest242,
	layoutPadTest243,
	layoutPadTest244,
	layoutPadTest245,
	layoutPadTest246,
	layoutPadTest247,
	layoutPadTest248,
	layoutPadTest249,
	layoutPadTest250,
	layoutPadTest251,
	layoutPadTest252,
	layoutPadTest253,
	layoutPadTest254,
	layoutPadTest255,
	layoutPadTest256,
	layoutPadTest257,
	layoutPadTest258,
	layoutPadTest259,
	layoutPadTest260,
	layoutPadTest261,
	layoutPadTest262,
	layoutPadTest263,
	layoutPadTest264,
	layoutPadTest265,
	layoutPadTest266,
	layoutPadTest267,
	layoutPadTest268,
	layoutPadTest269,
	layoutPadTest270,
	layoutPadTest271,
	layoutPadTest272,
	layoutPadTest273,
}

// TestLayoutControl never runs past its skip. It links what part 3's lock
// tests link from the standard library (exec.Cmd's pipes and
// bufio.Reader.ReadString), and keeps the pads linked.
func TestLayoutControl(t *testing.T) {
	t.Skip("layout control")
	var c exec.Cmd
	_, _ = c.StdinPipe()
	_, _ = c.StdoutPipe()
	_, _ = bufio.NewReader(nil).ReadString('\n')
	for _, pad := range layoutPads {
		pad()
	}
}

//go:noinline
func layoutPadTest0() {}

//go:noinline
func layoutPadTest1() {}

//go:noinline
func layoutPadTest2() {}

//go:noinline
func layoutPadTest3() {}

//go:noinline
func layoutPadTest4() {}

//go:noinline
func layoutPadTest5() {}

//go:noinline
func layoutPadTest6() {}

//go:noinline
func layoutPadTest7() {}

//go:noinline
func layoutPadTest8() {}

//go:noinline
func layoutPadTest9() {}

//go:noinline
func layoutPadTest10() {}

//go:noinline
func layoutPadTest11() {}

//go:noinline
func layoutPadTest12() {}

//go:noinline
func layoutPadTest13() {}

//go:noinline
func layoutPadTest14() {}

//go:noinline
func layoutPadTest15() {}

//go:noinline
func layoutPadTest16() {}

//go:noinline
func layoutPadTest17() {}

//go:noinline
func layoutPadTest18() {}

//go:noinline
func layoutPadTest19() {}

//go:noinline
func layoutPadTest20() {}

//go:noinline
func layoutPadTest21() {}

//go:noinline
func layoutPadTest22() {}

//go:noinline
func layoutPadTest23() {}

//go:noinline
func layoutPadTest24() {}

//go:noinline
func layoutPadTest25() {}

//go:noinline
func layoutPadTest26() {}

//go:noinline
func layoutPadTest27() {}

//go:noinline
func layoutPadTest28() {}

//go:noinline
func layoutPadTest29() {}

//go:noinline
func layoutPadTest30() {}

//go:noinline
func layoutPadTest31() {}

//go:noinline
func layoutPadTest32() {}

//go:noinline
func layoutPadTest33() {}

//go:noinline
func layoutPadTest34() {}

//go:noinline
func layoutPadTest35() {}

//go:noinline
func layoutPadTest36() {}

//go:noinline
func layoutPadTest37() {}

//go:noinline
func layoutPadTest38() {}

//go:noinline
func layoutPadTest39() {}

//go:noinline
func layoutPadTest40() {}

//go:noinline
func layoutPadTest41() {}

//go:noinline
func layoutPadTest42() {}

//go:noinline
func layoutPadTest43() {}

//go:noinline
func layoutPadTest44() {}

//go:noinline
func layoutPadTest45() {}

//go:noinline
func layoutPadTest46() {}

//go:noinline
func layoutPadTest47() {}

//go:noinline
func layoutPadTest48() {}

//go:noinline
func layoutPadTest49() {}

//go:noinline
func layoutPadTest50() {}

//go:noinline
func layoutPadTest51() {}

//go:noinline
func layoutPadTest52() {}

//go:noinline
func layoutPadTest53() {}

//go:noinline
func layoutPadTest54() {}

//go:noinline
func layoutPadTest55() {}

//go:noinline
func layoutPadTest56() {}

//go:noinline
func layoutPadTest57() {}

//go:noinline
func layoutPadTest58() {}

//go:noinline
func layoutPadTest59() {}

//go:noinline
func layoutPadTest60() {}

//go:noinline
func layoutPadTest61() {}

//go:noinline
func layoutPadTest62() {}

//go:noinline
func layoutPadTest63() {}

//go:noinline
func layoutPadTest64() {}

//go:noinline
func layoutPadTest65() {}

//go:noinline
func layoutPadTest66() {}

//go:noinline
func layoutPadTest67() {}

//go:noinline
func layoutPadTest68() {}

//go:noinline
func layoutPadTest69() {}

//go:noinline
func layoutPadTest70() {}

//go:noinline
func layoutPadTest71() {}

//go:noinline
func layoutPadTest72() {}

//go:noinline
func layoutPadTest73() {}

//go:noinline
func layoutPadTest74() {}

//go:noinline
func layoutPadTest75() {}

//go:noinline
func layoutPadTest76() {}

//go:noinline
func layoutPadTest77() {}

//go:noinline
func layoutPadTest78() {}

//go:noinline
func layoutPadTest79() {}

//go:noinline
func layoutPadTest80() {}

//go:noinline
func layoutPadTest81() {}

//go:noinline
func layoutPadTest82() {}

//go:noinline
func layoutPadTest83() {}

//go:noinline
func layoutPadTest84() {}

//go:noinline
func layoutPadTest85() {}

//go:noinline
func layoutPadTest86() {}

//go:noinline
func layoutPadTest87() {}

//go:noinline
func layoutPadTest88() {}

//go:noinline
func layoutPadTest89() {}

//go:noinline
func layoutPadTest90() {}

//go:noinline
func layoutPadTest91() {}

//go:noinline
func layoutPadTest92() {}

//go:noinline
func layoutPadTest93() {}

//go:noinline
func layoutPadTest94() {}

//go:noinline
func layoutPadTest95() {}

//go:noinline
func layoutPadTest96() {}

//go:noinline
func layoutPadTest97() {}

//go:noinline
func layoutPadTest98() {}

//go:noinline
func layoutPadTest99() {}

//go:noinline
func layoutPadTest100() {}

//go:noinline
func layoutPadTest101() {}

//go:noinline
func layoutPadTest102() {}

//go:noinline
func layoutPadTest103() {}

//go:noinline
func layoutPadTest104() {}

//go:noinline
func layoutPadTest105() {}

//go:noinline
func layoutPadTest106() {}

//go:noinline
func layoutPadTest107() {}

//go:noinline
func layoutPadTest108() {}

//go:noinline
func layoutPadTest109() {}

//go:noinline
func layoutPadTest110() {}

//go:noinline
func layoutPadTest111() {}

//go:noinline
func layoutPadTest112() {}

//go:noinline
func layoutPadTest113() {}

//go:noinline
func layoutPadTest114() {}

//go:noinline
func layoutPadTest115() {}

//go:noinline
func layoutPadTest116() {}

//go:noinline
func layoutPadTest117() {}

//go:noinline
func layoutPadTest118() {}

//go:noinline
func layoutPadTest119() {}

//go:noinline
func layoutPadTest120() {}

//go:noinline
func layoutPadTest121() {}

//go:noinline
func layoutPadTest122() {}

//go:noinline
func layoutPadTest123() {}

//go:noinline
func layoutPadTest124() {}

//go:noinline
func layoutPadTest125() {}

//go:noinline
func layoutPadTest126() {}

//go:noinline
func layoutPadTest127() {}

//go:noinline
func layoutPadTest128() {}

//go:noinline
func layoutPadTest129() {}

//go:noinline
func layoutPadTest130() {}

//go:noinline
func layoutPadTest131() {}

//go:noinline
func layoutPadTest132() {}

//go:noinline
func layoutPadTest133() {}

//go:noinline
func layoutPadTest134() {}

//go:noinline
func layoutPadTest135() {}

//go:noinline
func layoutPadTest136() {}

//go:noinline
func layoutPadTest137() {}

//go:noinline
func layoutPadTest138() {}

//go:noinline
func layoutPadTest139() {}

//go:noinline
func layoutPadTest140() {}

//go:noinline
func layoutPadTest141() {}

//go:noinline
func layoutPadTest142() {}

//go:noinline
func layoutPadTest143() {}

//go:noinline
func layoutPadTest144() {}

//go:noinline
func layoutPadTest145() {}

//go:noinline
func layoutPadTest146() {}

//go:noinline
func layoutPadTest147() {}

//go:noinline
func layoutPadTest148() {}

//go:noinline
func layoutPadTest149() {}

//go:noinline
func layoutPadTest150() {}

//go:noinline
func layoutPadTest151() {}

//go:noinline
func layoutPadTest152() {}

//go:noinline
func layoutPadTest153() {}

//go:noinline
func layoutPadTest154() {}

//go:noinline
func layoutPadTest155() {}

//go:noinline
func layoutPadTest156() {}

//go:noinline
func layoutPadTest157() {}

//go:noinline
func layoutPadTest158() {}

//go:noinline
func layoutPadTest159() {}

//go:noinline
func layoutPadTest160() {}

//go:noinline
func layoutPadTest161() {}

//go:noinline
func layoutPadTest162() {}

//go:noinline
func layoutPadTest163() {}

//go:noinline
func layoutPadTest164() {}

//go:noinline
func layoutPadTest165() {}

//go:noinline
func layoutPadTest166() {}

//go:noinline
func layoutPadTest167() {}

//go:noinline
func layoutPadTest168() {}

//go:noinline
func layoutPadTest169() {}

//go:noinline
func layoutPadTest170() {}

//go:noinline
func layoutPadTest171() {}

//go:noinline
func layoutPadTest172() {}

//go:noinline
func layoutPadTest173() {}

//go:noinline
func layoutPadTest174() {}

//go:noinline
func layoutPadTest175() {}

//go:noinline
func layoutPadTest176() {}

//go:noinline
func layoutPadTest177() {}

//go:noinline
func layoutPadTest178() {}

//go:noinline
func layoutPadTest179() {}

//go:noinline
func layoutPadTest180() {}

//go:noinline
func layoutPadTest181() {}

//go:noinline
func layoutPadTest182() {}

//go:noinline
func layoutPadTest183() {}

//go:noinline
func layoutPadTest184() {}

//go:noinline
func layoutPadTest185() {}

//go:noinline
func layoutPadTest186() {}

//go:noinline
func layoutPadTest187() {}

//go:noinline
func layoutPadTest188() {}

//go:noinline
func layoutPadTest189() {}

//go:noinline
func layoutPadTest190() {}

//go:noinline
func layoutPadTest191() {}

//go:noinline
func layoutPadTest192() {}

//go:noinline
func layoutPadTest193() {}

//go:noinline
func layoutPadTest194() {}

//go:noinline
func layoutPadTest195() {}

//go:noinline
func layoutPadTest196() {}

//go:noinline
func layoutPadTest197() {}

//go:noinline
func layoutPadTest198() {}

//go:noinline
func layoutPadTest199() {}

//go:noinline
func layoutPadTest200() {}

//go:noinline
func layoutPadTest201() {}

//go:noinline
func layoutPadTest202() {}

//go:noinline
func layoutPadTest203() {}

//go:noinline
func layoutPadTest204() {}

//go:noinline
func layoutPadTest205() {}

//go:noinline
func layoutPadTest206() {}

//go:noinline
func layoutPadTest207() {}

//go:noinline
func layoutPadTest208() {}

//go:noinline
func layoutPadTest209() {}

//go:noinline
func layoutPadTest210() {}

//go:noinline
func layoutPadTest211() {}

//go:noinline
func layoutPadTest212() {}

//go:noinline
func layoutPadTest213() {}

//go:noinline
func layoutPadTest214() {}

//go:noinline
func layoutPadTest215() {}

//go:noinline
func layoutPadTest216() {}

//go:noinline
func layoutPadTest217() {}

//go:noinline
func layoutPadTest218() {}

//go:noinline
func layoutPadTest219() {}

//go:noinline
func layoutPadTest220() {}

//go:noinline
func layoutPadTest221() {}

//go:noinline
func layoutPadTest222() {}

//go:noinline
func layoutPadTest223() {}

//go:noinline
func layoutPadTest224() {}

//go:noinline
func layoutPadTest225() {}

//go:noinline
func layoutPadTest226() {}

//go:noinline
func layoutPadTest227() {}

//go:noinline
func layoutPadTest228() {}

//go:noinline
func layoutPadTest229() {}

//go:noinline
func layoutPadTest230() {}

//go:noinline
func layoutPadTest231() {}

//go:noinline
func layoutPadTest232() {}

//go:noinline
func layoutPadTest233() {}

//go:noinline
func layoutPadTest234() {}

//go:noinline
func layoutPadTest235() {}

//go:noinline
func layoutPadTest236() {}

//go:noinline
func layoutPadTest237() {}

//go:noinline
func layoutPadTest238() {}

//go:noinline
func layoutPadTest239() {}

//go:noinline
func layoutPadTest240() {}

//go:noinline
func layoutPadTest241() {}

//go:noinline
func layoutPadTest242() {}

//go:noinline
func layoutPadTest243() {}

//go:noinline
func layoutPadTest244() {}

//go:noinline
func layoutPadTest245() {}

//go:noinline
func layoutPadTest246() {}

//go:noinline
func layoutPadTest247() {}

//go:noinline
func layoutPadTest248() {}

//go:noinline
func layoutPadTest249() {}

//go:noinline
func layoutPadTest250() {}

//go:noinline
func layoutPadTest251() {}

//go:noinline
func layoutPadTest252() {}

//go:noinline
func layoutPadTest253() {}

//go:noinline
func layoutPadTest254() {}

//go:noinline
func layoutPadTest255() {}

//go:noinline
func layoutPadTest256() {}

//go:noinline
func layoutPadTest257() {}

//go:noinline
func layoutPadTest258() {}

//go:noinline
func layoutPadTest259() {}

//go:noinline
func layoutPadTest260() {}

//go:noinline
func layoutPadTest261() {}

//go:noinline
func layoutPadTest262() {}

//go:noinline
func layoutPadTest263() {}

//go:noinline
func layoutPadTest264() {}

//go:noinline
func layoutPadTest265() {}

//go:noinline
func layoutPadTest266() {}

//go:noinline
func layoutPadTest267() {}

//go:noinline
func layoutPadTest268() {}

//go:noinline
func layoutPadTest269() {}

//go:noinline
func layoutPadTest270() {}

//go:noinline
func layoutPadTest271() {}

//go:noinline
func layoutPadTest272() {}

//go:noinline
func layoutPadTest273() {}
