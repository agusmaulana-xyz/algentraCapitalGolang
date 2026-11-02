# Algentra Capital

Portal lokal Algentra Capital menyediakan situs publik, katalog strategi, registrasi dan login client, dashboard client, serta console admin untuk mengelola akun utama dan copy trading MT5. Backend menggunakan Go, UI HTML/CSS/JavaScript, dan SQLite.

## Jalankan

1. Pasang Go 1.25 atau lebih baru.
2. Salin `.env.example` menjadi `.env`, lalu atur username admin dan password rahasia. Username admin 3–32 karakter; password minimal 12 karakter.
3. Jalankan `run.bat`. Jika MT5 ada di lokasi standar, runner akan menemukannya. Jika tidak, masukkan path `terminal64.exe` saat diminta.
4. Buka `http://127.0.0.1:8080` untuk situs publik. Daftar strategi ada di `/copy-trading`; login client ada di `/login`; console admin tersembunyi dari navigasi publik di `/admin/login`.

Server sengaja tetap bind ke loopback. Halaman publik dapat dicoba di komputer lokal, tetapi belum dipublikasikan ke internet.

Jika Windows menampilkan `blocked by your organization's Device Guard policy`, kebijakan organisasi menghentikan executable sebelum server mulai. Hubungi administrator TI untuk menyetujui `mt5-login.exe` melalui kebijakan organisasi. Jika kebijakan mensyaratkan tanda tangan, gunakan proses penandatanganan resmi organisasi.

Aplikasi selalu menjalankan pemeriksaan nyata melalui `terminal64.exe`; tidak ada mode simulasi. Terminal sumber disalin ke `data/terminals/` per akun, lalu dijalankan dalam `/portable`. Instalasi MT5 sumber tidak diubah. Pada pemeriksaan pertama, penyalinan executable sekitar 130 MB dapat memerlukan waktu.

## Integrasi dari `alok`

Probe ini mengambil alur dari `alok/cmd/copier-worker/login_probe.go`, `alok/mt5/experts/Algentra/MT5LoginProbe.mq5`, dan `alok/cmd/mt5-login-probe/main.go`:

1. Form mencari nama server broker melalui direktori HTTPS `mt5.mtapi.io`, seperti portal `alok`. Pilihan perusahaan broker ikut dipakai untuk membedakan server bernama sama. Backend hanya mengirim nama server publik ke direktori; login dan password tidak dikirim ke sana. Backend melewati alamat akses IP privat, lalu memakai nama server dari profil MT5 lokal. Jika daftar server lokal belum memuat broker, segarkan daftar lewat **File > Open an Account** di MT5.
2. Aplikasi menyiapkan terminal portable, menyalin `MT5LoginProbe.ex5`, mengaktifkan izin Expert pada `Config/common.ini`, lalu menjalankan terminal dalam process group dengan environment tanpa variabel password/secret/token.
3. Backend menulis konfigurasi startup sementara `[Common]` berisi `Login`, `Password`, dan `Server`, kemudian memulai terminal dengan `/portable` dan `/config:<file>`.
4. Startup menggunakan suffix mayoritas dari Market Watch bila tersedia (misalnya `AUDCADc` menghasilkan target `XAUUSDc`). Expert juga mencari varian XAUUSD langsung dari katalog simbol broker dan berpindah ke chart itu; tidak ada lagi terminal bootstrap terpisah yang membuka EURUSD. Hasil autentikasi memastikan chart aktual memakai simbol XAUUSD broker.
5. Terminal portable yang berhasil login tetap terbuka pada chart XAUUSD bersuffix broker. Pemeriksaan berikutnya akan menutup terminal portable yang masih dikelola aplikasi. Jika login gagal, proses terminal dihentikan setelah hasil dibaca. Log otorisasi juga membedakan akun/password ditolak, akun nonaktif, server tidak tersedia, dan masalah izin trading.

Sumber EA ada di `mt5/experts/Algentra/MT5LoginProbe.mq5`; binary `MT5LoginProbe.ex5` ikut disertakan dan tertanam di backend. Binary itu berhasil di-compile dengan MetaEditor dari MT5 yang terpasang pada komputer ini (0 error, 0 warning).

## Portal client dan admin

Halaman `/` dan `/copy-trading` dapat dibuka tanpa login. Client mendaftar lewat `/register`, masuk lewat `/login`, lalu menghubungkan satu akun MT5 dari `/dashboard`. Akun client selalu mulai standby; mode awal simulasi dan order live perlu dikonfirmasi secara eksplisit. Console master dan daftar client hanya dapat diakses oleh sesi admin di `/admin`.

Portal memakai database terpisah `data/algentra-portal.db` untuk user, hash password, sesi, dan tautan profil ke akun client. Database lama `data/mt5-login.db` tidak dimigrasikan. Admin login memakai `ALGENTRA_ADMIN_USERNAME` dan `ALGENTRA_ADMIN_PASSWORD` dari `.env`; password disimpan sebagai hash di memori proses, bukan di database. Sesi portal memakai cookie HttpOnly dan token CSRF. Password broker hanya dipakai untuk menjalankan terminal MT5 dan tidak disimpan oleh portal.

Label “Intelligence” di halaman publik tetap menampilkan ringkasan operasional berbasis aturan dari status koneksi dan mode akun. Gemini hanya dipakai pada halaman admin signal jika API key telah dikonfigurasi; aplikasi tidak menghasilkan prediksi pasar dan tidak menampilkan angka return rekaan.

## Copy trading akun utama dan client

Halaman lokal menyediakan form terpisah untuk menghubungkan satu akun utama dan sampai 50 akun client. Semua akun diverifikasi memakai terminal portable dan probe login yang sama. Terminal copy trading disimpan di `data/copy-trading/terminals/` agar terpisah dari pemeriksaan login manual.

Akun utama menjalankan `TelegramSignalEA`. Expert mengirim heartbeat dan snapshot posisi melalui API lokal, lalu backend meneruskan snapshot melalui bridge file lokal ke akun client yang menjalankan `MT5FollowerCopyEA`. Kredensial akun tidak dikirim ke layanan broker-directory atau server lain. Sistem mengikuti posisi market yang sudah terbuka, memperbarui stop-loss/take-profit, dan menutup posisi salinan saat posisi master ditutup. Pending order tidak disalin.

Akun client baru masuk status **standby**. Tombol **Mulai simulasi** menjalankan aturan lot tanpa mengirim order. Tombol **Aktifkan order live** meminta konfirmasi sebelum EA boleh membuka atau mengubah order di akun client. Mode lot tersedia sebagai rasio terhadap lot master atau lot tetap; setiap client juga memiliki batas maksimum lot per entry, total lot, dan jumlah posisi. Default awalnya rasio 1×, 0,10 lot per entry, 0,50 lot total, dan 10 posisi.

Aktivasi copy hanya mencakup entry master yang terjadi setelah client diaktifkan; posisi lama tidak dikejar. Snapshot master yang tidak segar membuat client menunggu, bukan menutup posisi client. Tombol jeda juga tidak menutup posisi yang sudah terbuka—posisi itu perlu dikelola di terminal client. Setelah aplikasi dimulai ulang, client kembali dijeda dan live copy perlu diaktifkan lagi.

Password broker hanya dipakai saat terminal disambungkan dan tidak disimpan di SQLite. MT5 dapat menyimpan sesi login sendiri di folder terminal portable. Token internal bridge untuk mengotorisasi client disimpan di database lokal; server aplikasi tetap bind ke loopback.

Console admin menyediakan hapus untuk riwayat pemeriksaan MT5, akun client, dan akun utama. Penghapusan menghentikan terminal yang dikelola, menghapus record database, dan membersihkan folder runtime yang dihitung khusus dari login+server akun itu. Menghapus akun client/master tidak menutup posisi atau pending order di broker; akun client lain tetap tersimpan, tetapi penghapusan master menjeda seluruh client.

## Manage Signal Telegram

Halaman `/admin/signals` hanya tersedia untuk sesi admin. Isi `TELEGRAM_API_ID` dan `TELEGRAM_API_HASH` dari aplikasi Telegram yang dibuat di `my.telegram.org/apps`, lalu isi `GEMINI_API_KEY` di `.env` dan mulai ulang aplikasi. Login Telegram memakai akun pengguna melalui kode verifikasi Telegram (dan password 2FA jika aktif); sesi disimpan di `data/telegram-user.gotd.session` dengan izin akses lokal terbatas. Pilih grup/channel yang menjadi sumber signal dari halaman admin.

Gemini mengekstrak signal terstruktur, tetapi aplikasi memvalidasi lagi aksi, simbol XAUUSD, entry zone, dan posisi SL sebelum menyimpan signal. Untuk format `XAUUSD SELL NOW 4054:::4063` dengan `SL 4070`, aplikasi menyiapkan dua SELL di 4054 dan 4063, dengan TP masing-masing 4038 dan 4056 (risk/reward 1:1). EA memilih LIMIT atau STOP berdasarkan harga pasar dan menghapus pending order setelah satu jam. Dua leg memerlukan akun MT5 hedging; akun netting akan ditolak EA.

Eksekusi live default nonaktif dan dimatikan lagi saat aplikasi dimulai ulang. Pengaktifan memerlukan Telegram, Gemini, grup sumber, akun master, dan heartbeat EA yang aktif. EA menggunakan 0.01 lot tetap dan validasi spread/margin dari broker; uji koneksi dan parsing lebih dulu, lalu uji order di akun demo sebelum memakai akun real. Jika live dimatikan, order yang telah ditempatkan di broker tidak otomatis dibatalkan.

MT5 mensyaratkan URL `http://127.0.0.1:8080` (atau URL lokal sesuai `MT5_LOGIN_ADDR`) ditambahkan manual melalui **Tools > Options > Expert Advisors > Allow WebRequest for listed URL** pada terminal master, dan AutoTrading harus aktif. Jika koneksi master pertama timeout, terminal portable dibiarkan terbuka agar URL dapat ditambahkan; setelah menyimpan pengaturan, hubungkan akun utama lagi. Data terminal portable ada di `data/copy-trading/terminals/`. Tanpa allowlist ini, EA tidak dapat mengirim snapshot, heartbeat, atau mengambil signal.

## Penyimpanan dan privasi

SQLite (`data/mt5-login.db`) menyimpan nomor login, nama server, status, pesan, waktu pemeriksaan, dan saldo beserta mata uang hanya setelah identitas login berhasil diverifikasi. Password tidak masuk ke SQLite, log aplikasi, API response, atau direktori pencarian broker. Untuk memulai ulang data terminal portabel dan sesi akun MT5, hentikan aplikasi lalu hapus folder `data/terminals/`.

Password harus diberikan sementara ke MT5 lewat file INI yang dibuat di direktori temp dan dihapus setelah terminal membaca konfigurasi. MT5 dapat menyimpan sesi/kredensialnya sendiri di folder terminal runtime, dan terminal tetap terbuka setelah autentikasi berhasil. Server web hanya bind ke loopback dan permintaan verifikasi hanya diterima dari browser lokal.

Probe ini hanya memverifikasi login dan kesiapan terminal; ia tidak mengirim order trading atau menjalankan backtest strategi. Halaman web menampilkan hasil login dan saldo akun; chart dibuka di terminal MT5.

Jika hasilnya menyebut **No connection**, terminal belum dapat menjangkau alamat server broker sehingga username/password belum diperiksa. Pastikan internet komputer aktif, akses MT5 tidak diblokir firewall/antivirus atau VPN, dan nama server persis seperti yang diberikan broker. Login ditolak broker akan ditampilkan terpisah sebagai `Invalid account` atau alasan otorisasi lain.

## Pemeriksaan kode

```powershell
go test ./...
go vet ./...
```

Test memakai probe terkendali untuk memeriksa penyimpanan hasil, kegagalan login, kecocokan identitas akun, serta bahwa password tidak tersimpan. Test tersebut tidak menggantikan percobaan login ke broker dengan kredensial akun yang valid.
