# CloudFlare R2
R2_ACCESS_KEY_ID=30d0092b24fc1ae2987967a74fba533d
R2_SECRET_ACCESS_KEY=166269c1fa483fc86bb446800dd095dfb425ee190ea61f4c9f009181fb94b382
R2_BUCKET=lmf-dev
R2_URL=https://pub-cdb9189dc4714a31a018906e010126e6.r2.dev
R2_ENDPOINT=https://633d4c207fbcb2a0c9b536bf4cf0e985.r2.cloudflarestorage.com
# Gmail SMTP

MAIL_MAILER=log
MAIL_HOST=smtp.gmail.com
MAIL_PORT=587
MAIL_USERNAME=vrajesh.logistic@gmail.com
MAIL_PASSWORD="eydl zzpv quna bkpx"
MAIL_FROM_ADDRESS=vrajesh.logistic@gmail.com
MAIL_FROM_NAME="goravel e-commerce"

# Run in Local Mac Machine
mkdir -p dev/r2 && chmod 700 dev/r2
make build
PGTIJORI_DEV=1 ./pgtijori serve --config dev/r2/config.yaml --state dev/r2/state.db --log dev/r2/pgtijori.log

# Check in Second Terminal
P="--config dev/r2/config.yaml --state dev/r2/state.db --log dev/r2/pgtijori.log"
PGTIJORI_DEV=1 ./pgtijori status $P
PGTIJORI_DEV=1 ./pgtijori dumps list --config dev/r2/config.yaml --state dev/r2/state.db
PGTIJORI_DEV=1 ./pgtijori run report $P          # the daily email, to your real inbox
pgbackrest --config=/opt/homebrew/etc/pgbackrest/pgbackrest.conf --stanza=mac-test info

# Clean up when you're done
Turn off archiving, otherwise your Mac keeps sending WAL to R2 whenever PostgreSQL runs:

rm /opt/homebrew/var/postgresql@18/conf.d/10-pgbackrest.conf
brew services restart postgresql@18
Delete the test data from the bucket in the R2 dashboard.