package expr

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestCombined(t *testing.T) {
	e, err := JS(`MULT(AVG(SUB(ADD(DIV("a", "b"), 1), 0.5)), 2)`)
	if !assert.NoError(t, err, "Unable to parse JS expression") {
		return
	}
	params1 := Map{
		"a": Float(8.8),
		"b": Float(4.4),
	}
	params2 := Map{
		"a": Float(20),
		"b": Float(5),
	}
	params3 := Map{
		"a": Float(0),
		"b": Float(1),
	}

	assert.Equal(t, []string{"a", "b"}, e.DependsOn())
	a := e.Accumulator()
	a.Update(params1)
	a.Update(params2)
	assertFloatEquals(t, 7, a.Get())

	rta := e.Accumulator()
	rta.InitFrom(a.Bytes())
	assertFloatEquals(t, 7, rta.Get())

	rta.Update(params3)
	assertFloatEquals(t, 5, rta.Get())
}
