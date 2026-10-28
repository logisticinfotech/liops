# Step 1
brew install --cask multipass
multipass launch 24.04 --name pgrestore --cpus 2 --memory 4G --disk 16G
multipass shell pgrestore          # you're now root-capable `ubuntu` user in the VM

# Step2
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/liops-linux-arm64 .
multipass transfer dist/liops-linux-arm64 pgrestore:/home/ubuntu/liops

# Step3
sudo apt update && sudo apt install -y postgresql-common
sudo /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
sudo apt install -y postgresql-18 pgbackrest     # match your server's major version
sudo install -m 755 ~/liops /usr/local/bin/liops

# Step4

	Real backups in a cloud bucket: the VM has internet access, so point it at the same bucket and backup name (stanza) as the server, using a config copied from the server. Treat the VM as restore-only. Don't run setup or let it start archiving, or it will write to that server's folder. CheckOwner should refuse anyway, but don't count on it.

	Your local dev MinIO (make dev-up): from inside the VM, the Mac is usually reachable at 192.168.64.1. Check with ip route | grep default in the VM. MinIO must listen on all interfaces, not just 127.0.0.1.

# Step5 Cleanup
multipass stop pgrestore
multipass delete --purge pgrestore

# Step6 Access Postgres from Mac
ip addr # then replace ipaddress range below

sudo sed -i "s/^#listen_addresses.*/listen_addresses = '*'/" /etc/postgresql/18/main/postgresql.conf
echo "host all all 192.168.252.0/24 scram-sha-256" | sudo tee -a /etc/postgresql/18/main/pg_hba.conf
sudo -u postgres psql -c "ALTER USER postgres PASSWORD 'devpass';"
sudo systemctl restart postgresql
