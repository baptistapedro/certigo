/*-
 * Copyright 2017 Square Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package starttls

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"time"

	"github.com/square/certigo/starttls/ldap"
	"github.com/square/certigo/starttls/mysql"
	"github.com/square/certigo/starttls/psql"
)

type connectResult struct {
	state *tls.ConnectionState
	err   error
}

func tlsConfigForConnect(connectName, clientCert, clientKey string) (*tls.Config, error) {
	conf := &tls.Config{
		// We verify later manually so we can print results
		InsecureSkipVerify: true,
		ServerName:         connectName,
		MinVersion:         tls.VersionSSL30,
	}

	if clientCert != "" {
		keyFile := clientCert
		if clientKey != "" {
			keyFile = clientKey
		}

		cert, err := tls.LoadX509KeyPair(clientCert, keyFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read client certificate/key: %s\n", err)
		}

		conf.Certificates = []tls.Certificate{cert}
	}

	return conf, nil
}

// GetConnectionState connects to a TLS server, returning the connection state.
// Currently, startTLSType can be one of "mysql", "postgres" or "psql", or the
// empty string, which does a normal TLS connection. connectTo specifies the
// address to connect to. connectName sets SNI.  connectCert and connectKey are
// client certs
func GetConnectionState(startTLSType, connectName, connectTo, clientCert, clientKey string, timeout time.Duration) (*tls.ConnectionState, error) {
	var state *tls.ConnectionState
	var err error
	var tlsConfig *tls.Config

	// Network dialer to use (if possible)
	dialer := net.Dialer{
		Timeout:  timeout,
		Deadline: time.Now().Add(timeout),
	}

	// Never take longer than timeout
	res := make(chan connectResult, 1)
	go func() {
		<-time.After(timeout)
		res <- connectResult{nil, errors.New("timed out")}
	}()

	switch startTLSType {
	case "postgres", "psql":
		// No tlsConfig needed for postgres, but all others do.
	default:
		tlsConfig, err = tlsConfigForConnect(connectName, clientCert, clientKey)
		if err != nil {
			return nil, err
		}
	}

	go func() {
		switch startTLSType {
		case "":
			conn, err := tls.DialWithDialer(&dialer, "tcp", connectTo, tlsConfig)
			if err != nil {
				res <- connectResult{nil, err}
				return
			}
			defer conn.Close()
			state := conn.ConnectionState()
			res <- connectResult{&state, nil}
		case "ldap":
			l, err := ldap.Dial("tcp", connectTo, timeout)
			if err != nil {
				res <- connectResult{nil, err}
				return
			}
			defer l.Close()

			err = l.StartTLS(tlsConfig)
			if err != nil {
				res <- connectResult{nil, err}
				return
			}
			state, err = l.TLSConnectionState()
			if err != nil {
				res <- connectResult{nil, fmt.Errorf("LDAP connection isn't TLS after StartTLS: %s", err.Error())}
				return
			}
			res <- connectResult{state, nil}
		case "mysql":
			mysql.RegisterTLSConfig("certigo", tlsConfig)
			state, err = mysql.DumpTLS(fmt.Sprintf("certigo@tcp(%s)/?tls=certigo&timeout=%s", connectTo, timeout.String()))
			if err != nil {
				res <- connectResult{nil, err}
				return
			}
			res <- connectResult{state, nil}
		case "postgres", "psql":
			// Setting sslmode to "require" skips verification.
			url := fmt.Sprintf("postgres://certigo@%s/?sslmode=require&connect_timeout=%d", connectTo, timeout/time.Second)
			if clientCert != "" {
				url += fmt.Sprintf("&sslcert=%s", clientCert)
			}
			if clientKey != "" {
				url += fmt.Sprintf("&sslkey=%s", clientCert)
			}
			state, err = pq.DumpTLS(url)
			if err != nil {
				res <- connectResult{nil, err}
				return
			}
			res <- connectResult{state, nil}
		case "smtp":
			// Go's net/smtp doesn't support timeouts, so if we hit a timeout we might
			// leak a Go routine (at least until we hit a lower-level TCP timeout or such).
			// This is not an issue for Certigo since it's just a short-lived CLI utility.
			client, err := smtp.Dial(connectTo)
			if err != nil {
				res <- connectResult{nil, err}
				return
			}
			err = client.StartTLS(tlsConfig)
			if err != nil {
				res <- connectResult{nil, err}
				return
			}
			state, ok := client.TLSConnectionState()
			if !ok {
				res <- connectResult{nil, errors.New("SMTP connection isn't TLS after StartTLS")}
			}
			res <- connectResult{&state, nil}
		case "ftp":
			state, err = dumpAuthTLSFromFTP(&dialer, connectTo, tlsConfig)
			res <- connectResult{state, err}
		default:
			res <- connectResult{nil, fmt.Errorf("unknown StartTLS protocol: %s", startTLSType)}
		}
	}()

	result := <-res

	if result.err != nil {
		return nil, fmt.Errorf("error connecting: %v\n", result.err)
	}

	return result.state, nil
}