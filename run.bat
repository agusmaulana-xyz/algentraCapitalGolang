@echo off
setlocal
cd /d "%~dp0"

if "%MT5_LOGIN_ADDR%"=="" set "MT5_LOGIN_ADDR=127.0.0.1:8080"
if "%MT5_LOGIN_DB%"=="" set "MT5_LOGIN_DB=data\mt5-login.db"
if "%GOCACHE%"=="" set "GOCACHE=%~dp0.cache\go-build"

set "MT5_ALREADY_RUNNING="
for /f "delims=" %%A in ('powershell.exe -NoProfile -NonInteractive -Command "try { $algentraHealth = Invoke-RestMethod -Uri http://%MT5_LOGIN_ADDR%/api/health -TimeoutSec 2; if ($algentraHealth.ok) { Write-Output RUNNING } } catch { }"') do set "MT5_ALREADY_RUNNING=%%A"
if "%MT5_ALREADY_RUNNING%"=="RUNNING" (
  echo Algentra Capital sudah berjalan di http://%MT5_LOGIN_ADDR%
  echo Buka alamat tersebut; jangan jalankan server kedua.
  exit /b 0
)

if not defined MT5_TERMINAL_PATH if exist "C:\Program Files\MetaTrader 5\terminal64.exe" set "MT5_TERMINAL_PATH=C:\Program Files\MetaTrader 5\terminal64.exe"
if not defined MT5_TERMINAL_PATH set /p "MT5_TERMINAL_PATH=Masukkan path terminal64.exe: "
if not defined MT5_TERMINAL_PATH (
  echo Path terminal64.exe wajib diisi untuk verifikasi broker.
  pause
  exit /b 1
)
if not exist "%MT5_TERMINAL_PATH%" (
  echo terminal64.exe tidak ditemukan: %MT5_TERMINAL_PATH%
  pause
  exit /b 1
)

where go.exe >nul 2>nul
if errorlevel 1 (
  echo Go belum terpasang. Pasang Go 1.25 atau lebih baru, lalu jalankan run.bat lagi.
  pause
  exit /b 1
)

echo Menjalankan Algentra Capital pada http://%MT5_LOGIN_ADDR%
echo Terminal: %MT5_TERMINAL_PATH%
echo Menyiapkan aplikasi Go dan SQLite...
go build -o mt5-login.exe .
if errorlevel 1 (
  echo.
  echo Aplikasi gagal di-build. Periksa dependensi Go dan koneksi internet.
  pause
  exit /b 1
)

echo Tekan Ctrl+C untuk menghentikan server.
mt5-login.exe
set "APP_EXIT_CODE=%ERRORLEVEL%"
if not "%APP_EXIT_CODE%"=="0" (
  echo.
  echo Aplikasi tidak berhasil dijalankan atau berhenti dengan kode %APP_EXIT_CODE%.
  if "%APP_EXIT_CODE%"=="1073751882" (
    echo Windows Device Guard / App Control memblokir executable ini sebelum server mulai.
    echo Berikan SHA-256 berikut kepada administrator TI untuk persetujuan melalui policy organisasi:
    certutil -hashfile "mt5-login.exe" SHA256
    echo Persetujuan berdasarkan penanda tangan organisasi lebih sesuai untuk build berikutnya.
  ) else (
    echo Jika Windows mencatat Code Integrity event 3077, minta administrator TI memeriksa policy pemblokiran.
  )
  pause
  exit /b 1
)
