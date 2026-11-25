@echo off
setlocal EnableExtensions EnableDelayedExpansion
chcp 65001 >nul 2>&1
cd /d "%~dp0"

set "ROOT=%CD%"
set "RUNTIME=%ROOT%\.build-tmp"
set "GO_DIR=%RUNTIME%\tools\go"
set "PG_DIR=%RUNTIME%\tools\pgsql"
set "PGDATA=%RUNTIME%\pgdata"
set "PG_LOG=%RUNTIME%\postgres.log"
set "PG_PASSWORD_FILE=%RUNTIME%\postgres-password.txt"
set "PG_PORT=5433"
set "GO_VERSION=1.26.3"
set "GO_ZIP=go%GO_VERSION%.windows-amd64.zip"
set "GO_URL=https://go.dev/dl/%GO_ZIP%"
set "GO_SHA256=20d2ceafb4ed41b96b879010927b28bc92a5be57a7c1801ce365a9ca51d3224a"
set "PG_URL=https://sbp.enterprisedb.com/getfile.jsp?fileid=1260731"
set "PG_ZIP=postgresql-16-windows-x64-binaries.zip"
set "BIN=%ROOT%\pojun.exe"
set "APP_URL=http://127.0.0.1:8787"
set "BUILD=0"
set "MANAGED_DB_STARTED=0"

if /i "%~1"=="--build" (
    set "BUILD=1"
    shift
)
set "APP_ARGS=%*"
if not defined APP_ARGS set "APP_ARGS=-addr 127.0.0.1:8787"

echo [Pojun] Project: %ROOT%
where powershell.exe >nul 2>&1
if errorlevel 1 goto :missing_powershell
where curl.exe >nul 2>&1
if errorlevel 1 goto :missing_curl

if not exist "%BIN%" set "BUILD=1"
if "%BUILD%"=="1" (
    call :build_app
    if errorlevel 1 exit /b 1
)
if not exist "%BIN%" goto :build_failed

if defined POJUN_PG_DSN goto :database_ready
if exist "%PGDATA%\managed-by-pojun.txt" (
    if not exist "%ROOT%\config.json" (
        call :setup_local_database
        if errorlevel 1 goto :database_failed
    )
    goto :start_local_database
)
if exist "%ROOT%\config.json" goto :database_ready
call :setup_local_database
if errorlevel 1 goto :database_failed

:start_local_database
"%PG_DIR%\bin\pg_isready.exe" -h 127.0.0.1 -p %PG_PORT% >nul 2>&1
if not errorlevel 1 goto :database_ready
set /p PG_PASSWORD=<"%PG_PASSWORD_FILE%"
"%PG_DIR%\bin\pg_ctl.exe" -D "%PGDATA%" -l "%PG_LOG%" -o "-h 127.0.0.1 -p %PG_PORT%" -w start
if errorlevel 1 goto :database_failed
set "MANAGED_DB_STARTED=1"

:database_ready
echo [Pojun] Starting %BIN%
echo [Pojun] Console: %APP_URL%
echo [Pojun] The browser opens automatically after the health check passes.
echo [Pojun] Keep this window open. Ctrl-C stops the application.
start "" powershell.exe -NoProfile -WindowStyle Hidden -Command "$deadline=(Get-Date).AddSeconds(90); do { try { $r=Invoke-WebRequest -UseBasicParsing -Uri 'http://127.0.0.1:8787/api/health' -TimeoutSec 2; if($r.StatusCode -eq 200){ Start-Process $env:APP_URL; exit 0 } } catch {}; Start-Sleep -Milliseconds 500 } while((Get-Date) -lt $deadline); exit 1"
set /a crashes=0
set /a delay=1

:run_app
"%BIN%" %APP_ARGS%
set "code=!ERRORLEVEL!"
if "!code!"=="0" goto :stopped
if "!code!"=="75" (
    echo [Pojun] Restart requested by the application.
    set /a crashes=0
    set /a delay=1
    goto :run_app
)
set /a crashes+=1
if !crashes! geq 5 (
    echo [Pojun] Application exited 5 times consecutively ^(code=!code!^).
    echo [Pojun] Check the error above, config.json, and .build-tmp\postgres.log.
    goto :stopped
)
echo [Pojun] Application exited unexpectedly ^(code=!code!^); retrying in !delay!s.
ping -n !delay! 127.0.0.1 >nul 2>&1
set /a delay*=2
if !delay! gtr 60 set /a delay=60
goto :run_app

:stopped
if "%MANAGED_DB_STARTED%"=="1" (
    "%PG_DIR%\bin\pg_ctl.exe" -D "%PGDATA%" -m fast -w stop >nul 2>&1
)
echo [Pojun] Stopped.
endlocal
exit /b %code%

:build_app
echo [Pojun] Building the Windows executable with the embedded frontend.
where node.exe >nul 2>&1
if errorlevel 1 goto :missing_node
where npm.cmd >nul 2>&1
if errorlevel 1 goto :missing_npm

echo [Pojun] Installing frontend dependencies.
pushd "%ROOT%\web"
if not exist "%ROOT%\web\node_modules" call npm ci
if errorlevel 1 (
    popd
    exit /b 1
)
echo [Pojun] Exporting the frontend.
set "NEXT_EXPORT=1"
call npx.cmd next build
set "BUILD_ERROR=!ERRORLEVEL!"
set "NEXT_EXPORT="
popd
if not "!BUILD_ERROR!"=="0" exit /b !BUILD_ERROR!
if not exist "%ROOT%\server\webui\dist" mkdir "%ROOT%\server\webui\dist"
robocopy "%ROOT%\web\out" "%ROOT%\server\webui\dist" /E /NFL /NDL /NJH /NJS /NP >nul
if errorlevel 8 (
    echo [Pojun] Failed to copy frontend assets.
    exit /b 1
)

if not exist "%GO_DIR%\bin\go.exe" (
    if not exist "%RUNTIME%\downloads" mkdir "%RUNTIME%\downloads"
    if not exist "%RUNTIME%\tools" mkdir "%RUNTIME%\tools"
    echo [Pojun] Downloading Go %GO_VERSION% ^(portable, no system installation^).
    curl.exe -fL --retry 3 --connect-timeout 20 -o "%RUNTIME%\downloads\%GO_ZIP%" "%GO_URL%"
    if errorlevel 1 (
        echo [Pojun] Go download failed.
        exit /b 1
    )
    powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$h=(Get-FileHash -Algorithm SHA256 '%RUNTIME%\downloads\%GO_ZIP%').Hash; if($h -ne '%GO_SHA256%'){throw 'Go checksum mismatch'}; Expand-Archive -Path '%RUNTIME%\downloads\%GO_ZIP%' -DestinationPath '%RUNTIME%\tools' -Force"
    if errorlevel 1 (
        echo [Pojun] Go verification or extraction failed.
        exit /b 1
    )
)

set "PATH=%GO_DIR%\bin;%PATH%"
set "CGO_ENABLED=0"
set "POJUN_BUILD_VERSION=dev"
where git.exe >nul 2>&1
if not errorlevel 1 (
    for /f "delims=" %%v in ('git describe --tags --always --dirty 2^>nul') do set "POJUN_BUILD_VERSION=%%v"
)
echo [Pojun] Compiling Go backend.
go build -tags embedui -trimpath -ldflags "-s -w -buildid= -X main.version=%POJUN_BUILD_VERSION%" -o "%BIN%" ./cmd/pojun
if errorlevel 1 (
    echo [Pojun] Go build failed.
    exit /b 1
)
echo [Pojun] Build complete: %BIN%
exit /b 0

:setup_local_database
echo [Pojun] Preparing portable PostgreSQL 16 on 127.0.0.1:%PG_PORT%.
if not exist "%RUNTIME%" mkdir "%RUNTIME%"
if not exist "%PG_DIR%\bin\postgres.exe" (
    if not exist "%RUNTIME%\downloads" mkdir "%RUNTIME%\downloads"
    if not exist "%RUNTIME%\tools" mkdir "%RUNTIME%\tools"
    echo [Pojun] Downloading PostgreSQL binaries.
    curl.exe -fL --retry 3 --connect-timeout 20 -o "%RUNTIME%\downloads\%PG_ZIP%" "%PG_URL%"
    if errorlevel 1 (
        echo [Pojun] PostgreSQL download failed.
        exit /b 1
    )
    powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "Expand-Archive -Path '%RUNTIME%\downloads\%PG_ZIP%' -DestinationPath '%RUNTIME%\tools' -Force"
    if errorlevel 1 (
        echo [Pojun] PostgreSQL extraction failed.
        exit /b 1
    )
)
if not exist "%PG_PASSWORD_FILE%" (
    powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$p=[Guid]::NewGuid().ToString('N')+[Guid]::NewGuid().ToString('N'); [IO.File]::WriteAllText('%PG_PASSWORD_FILE%',$p,(New-Object Text.UTF8Encoding($false)))"
    if errorlevel 1 exit /b 1
)
set /p PG_PASSWORD=<"%PG_PASSWORD_FILE%"
if not exist "%PGDATA%\PG_VERSION" (
    "%PG_DIR%\bin\initdb.exe" -D "%PGDATA%" -U postgres -A scram-sha-256 --pwfile="%PG_PASSWORD_FILE%" -E UTF8 --no-locale
    if errorlevel 1 (
        echo [Pojun] Database initialization failed.
        exit /b 1
    )
)
echo managed-by-pojun>"%PGDATA%\managed-by-pojun.txt"
"%PG_DIR%\bin\pg_isready.exe" -h 127.0.0.1 -p %PG_PORT% >nul 2>&1
if errorlevel 1 (
    "%PG_DIR%\bin\pg_ctl.exe" -D "%PGDATA%" -l "%PG_LOG%" -o "-h 127.0.0.1 -p %PG_PORT%" -w start
    if errorlevel 1 (
        echo [Pojun] Database startup failed. See %PG_LOG%.
        exit /b 1
    )
    set "MANAGED_DB_STARTED=1"
)
set "PGPASSWORD=%PG_PASSWORD%"
set "SQL=DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pojun') THEN CREATE ROLE pojun LOGIN PASSWORD '%PG_PASSWORD%'; ELSE ALTER ROLE pojun WITH LOGIN PASSWORD '%PG_PASSWORD%'; END IF; END $$;"
"%PG_DIR%\bin\psql.exe" -h 127.0.0.1 -p %PG_PORT% -U postgres -d postgres -v ON_ERROR_STOP=1 -c "!SQL!"
if errorlevel 1 goto :database_failed
set "DB_EXISTS="
"%PG_DIR%\bin\psql.exe" -h 127.0.0.1 -p %PG_PORT% -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = 'pojun'" > "%RUNTIME%\db-exists.txt"
if errorlevel 1 goto :database_failed
set /p DB_EXISTS=<"%RUNTIME%\db-exists.txt"
del "%RUNTIME%\db-exists.txt" >nul 2>&1
if not defined DB_EXISTS (
    "%PG_DIR%\bin\createdb.exe" -h 127.0.0.1 -p %PG_PORT% -U postgres -O pojun pojun
    if errorlevel 1 goto :database_failed
)
if not exist "%ROOT%\config.json" (
    powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$c=@{database=@{dsn='postgres://pojun:'+$env:PG_PASSWORD+'@127.0.0.1:%PG_PORT%/pojun?sslmode=disable'}}; $j=$c | ConvertTo-Json -Depth 4; [IO.File]::WriteAllText('%ROOT%\config.json',$j,(New-Object Text.UTF8Encoding($false)))"
    if errorlevel 1 goto :database_failed
)
echo [Pojun] Local database ready.
exit /b 0

:missing_powershell
echo [Pojun] Windows PowerShell is required.
exit /b 1
:missing_curl
echo [Pojun] curl.exe is required. Update Windows 10/11 or install a newer Windows SDK.
exit /b 1
:missing_node
echo [Pojun] Node.js is required to build the frontend: https://nodejs.org/en/download/
exit /b 1
:missing_npm
echo [Pojun] npm is required to build the frontend. Reinstall Node.js from https://nodejs.org/en/download/
exit /b 1
:build_failed
echo [Pojun] pojun.exe was not produced.
exit /b 1
:database_failed
echo [Pojun] Database setup failed. See %PG_LOG%.
if "%MANAGED_DB_STARTED%"=="1" "%PG_DIR%\bin\pg_ctl.exe" -D "%PGDATA%" -m fast -w stop >nul 2>&1
exit /b 1
