package destination

import (
	"net"
	"strconv"
	"veil.local/core/endpoint"
)

type Address struct {
	Host string
	Port uint16
}

func Parse(host string, port uint16) (Address, error) {
	e, err := endpoint.Parse(host, port)
	if err != nil {
		return Address{}, err
	}
	return Address{e.Host(), e.Port()}, nil
}
func (a Address) String() string { return net.JoinHostPort(a.Host, strconv.Itoa(int(a.Port))) }
func (a Address) Valid() bool    { v, err := Parse(a.Host, a.Port); return err == nil && v == a }
