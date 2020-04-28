package redimo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBasicSets(t *testing.T) {
	c := newClient(t)

	err := c.SADD("s1", "m1", "m2", "m3")
	assert.NoError(t, err)

	ok, err := c.SISMEMBER("s1", "m1")
	assert.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.SISMEMBER("s1", "nonexistentmember")
	assert.NoError(t, err)
	assert.False(t, ok)

	members, err := c.SMEMBERS("s1")
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"m1", "m2", "m3"}, members)

	members, err = c.SMEMBERS("nosuchset")
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{}, members)

	err = c.SREM("s1", "m1", "m2")
	assert.NoError(t, err)

	members, err = c.SMEMBERS("s1")
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"m3"}, members)

	ok, err = c.SISMEMBER("s1", "m1")
	assert.NoError(t, err)
	assert.False(t, ok)
}
