package spf

import (
	"context"
	"errors"
	"net"
	"strings"
)

// netResolver 把标准库 net.Resolver 适配为 SPF 求值所需的最小接口。
type netResolver struct{}

func (netResolver) LookupTXT(ctx context.Context, domain string) ([]string, error) {
	txts, err := net.DefaultResolver.LookupTXT(ctx, domain)
	if err != nil {
		return nil, mapDNSError(err)
	}
	return txts, nil
}

func (netResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	// "udp" 同时取 A 与 AAAA。
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, mapDNSError(err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

func (netResolver) LookupMXHosts(ctx context.Context, domain string) ([]string, error) {
	mxs, err := net.DefaultResolver.LookupMX(ctx, domain)
	if err != nil {
		return nil, mapDNSError(err)
	}
	hosts := make([]string, 0, len(mxs))
	for _, mx := range mxs {
		hosts = append(hosts, strings.TrimSuffix(mx.Host, "."))
	}
	return hosts, nil
}

// mapDNSError 把标准库的 NXDOMAIN 映射为 ErrNXDOMAIN，其余原样返回。
func mapDNSError(err error) error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return ErrNXDOMAIN
	}
	return err
}
