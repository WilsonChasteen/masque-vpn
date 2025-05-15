# CA Certificate Installation Guide

## Windows Installation
1. Double-click the `ca.pem` file
2. Select "Install Certificate"
3. Choose "Local Machine" storage location
4. Select "Place all certificates in the following store"
5. Browse to "Trusted Root Certification Authorities" and finish

## macOS Installation
```bash
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ca.pem
```

## Linux Installation
```bash
sudo cp ca.pem /usr/local/share/ca-certificates/MasqueVPN-CA.crt
sudo update-ca-certificates
```

## Browser Trust Configuration
**Google Chrome/Edge:**
1. Navigate to chrome://settings/security
2. Click "Manage certificates"
3. Import `ca.pem` into "Trusted Root Certification Authorities"

**Mozilla Firefox:**
1. Go to about:preferences#privacy
2. Scroll to Certificates > View Certificates
3. Import `ca.pem` under "Authorities" tab