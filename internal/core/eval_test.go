package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func resetSetStore() { ResetStores() }

func TestCmdSADD(t *testing.T) {
	resetSetStore()
	res, err := Decode(cmdSADD([]string{"set", "adele"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 1, res)

	res, err = Decode(cmdSADD([]string{"set", "adele", "bob", "chris"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 2, res)
}

func TestCmdSREM(t *testing.T) {
	resetSetStore()
	res, err := Decode(cmdSREM([]string{"set", "adele"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 0, res)

	cmdSADD([]string{"set", "a", "b", "c"})
	res, err = Decode(cmdSREM([]string{"set", "a", "d"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 1, res)
}

func TestCmdSCARD(t *testing.T) {
	resetSetStore()

	cmdSADD([]string{"set", "a", "b", "c"})
	res, err := Decode(cmdSCARD([]string{"set"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 3, res)
}

func TestCmdSMEMBERS(t *testing.T) {
	resetSetStore()

	cmdSADD([]string{"set", "a", "b", "c"})
	res, err := Decode(cmdSMEMBERS([]string{"set"}))
	assert.Nil(t, err)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, res)
}

func TestCmdSMISMEMBER(t *testing.T) {
	resetSetStore()

	cmdSADD([]string{"set", "a", "b", "c"})
	res, err := Decode(cmdSMISMEMBER([]string{"set", "a", "d"}))
	assert.Nil(t, err)
	assert.ElementsMatch(t, []int{1, 0}, res)
}

func TestCmdSRAND(t *testing.T) {
	resetSetStore()

	cmdSADD([]string{"set", "a", "b", "c"})
	res, err := Decode(cmdSRAND([]string{"set", "2"}))

	assert.Nil(t, err)
	m := make(map[string]struct{})
	m["a"] = struct{}{}
	m["b"] = struct{}{}
	m["c"] = struct{}{}
	rd := make(map[string]struct{})
	for _, key := range res.([]interface{}) {
		k := key.(string)
		assert.Contains(t, m, k, "key must be in set")
		assert.NotContains(t, m, rd, "key must be not duplicated")
		rd[k] = struct{}{}
	}
}

func TestCmdSPOP(t *testing.T) {
	resetSetStore()

	cmdSADD([]string{"set", "a", "b", "c"})
	res, err := Decode(cmdSPOP([]string{"set", "2"}))

	assert.Nil(t, err)
	m := make(map[string]struct{})
	m["a"] = struct{}{}
	m["b"] = struct{}{}
	m["c"] = struct{}{}
	for _, key := range res.([]interface{}) {
		k := key.(string)
		delete(m, k)
	}
	var expected []string
	for k := range m {
		expected = append(expected, k)
	}

	res, err = Decode(cmdSMEMBERS([]string{"set"}))
	assert.ElementsMatch(t, expected, res)
}
