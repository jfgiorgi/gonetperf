This program measures the speed of the Golang http server (and client) using HTTP/1.1 (no tls)

It also allows to test client context deadline and server timeouts behavior (context)

usage: run with no argument to do a test on loopback interface with 10G of data

use the "-s" option to be in server only mode and use another program like curl (or https://nspeed.app) to test
locally or over the wire. you can also use the same program as a remote client with the "-c url" option

use the "-t duration" to limit the test duration from the client side (which is on at 8 seconds by default)

use the "-st duration" to limit the test duration from the server side.

see "-h" also for more help.
