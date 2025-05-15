#!/bin/sh


# Generate CA key and self-signed certificate
cfssl genkey -initca ca-csr.json | cfssljson -bare ca