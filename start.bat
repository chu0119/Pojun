@echo off
rem Pojun - Windows launcher (Docker Desktop / WSL2 backend)
rem
rem Usage (double-click or command line):
rem   start.bat            start containers and open the console
rem   start.bat status     show container status
rem   start.bat logs       follow logs (Ctrl-C to exit)
rem   start.bat stop       stop containers
rem   start.bat restart    restart containers
rem
rem This script does not download or build anything: it only calls Docker
rem Compose to run the prebuilt image. Native Windows exe is not supported
rem (hostd / apt toolchain / systemd are Linux-only).

setlocal EnableExtensions EnableDelayedExpansion
chcp 65001 >nul 2>&1
cd /d "%~dp0"

set "COMPOSE=docker compose -f docker-compose.yml -f docker-compose.windows.yml"
set "URL=http://127.0.0.1:8787"

if /i "%~1"=="stop" goto :stop
if /i "%~1"=="logs" goto :logs
if /i "%~1"=="status" goto :status
if /i "%~1"=="restart" goto :restart
if /i "%~1"=="help" goto :help
if /i "%~1"=="--help" goto :help

:start
call :ensure_docker
if errorlevel 1 exit /b 1
call :ensure_env
if errorlevel 1 exit /b 1

echo [Pojun] Starting containers (first run may pull the image)...
%COMPOSE% up -d
if errorlevel 1 goto :compose_failed

call :wait_health
if errorlevel 1 (
	echo [Pojun] Health check timed out. Check logs with:
	echo   start.bat logs
	pause
	exit /b 1
)

echo [Pojun] Console: %URL%
start "" "%URL%"
echo [Pojun] Started. "start.bat logs" follows logs; "start.bat stop" stops containers.
pause
exit /b 0

:stop
call :ensure_docker
if errorlevel 1 exit /b 1
%COMPOSE% down
if errorlevel 1 goto :compose_failed
echo [Pojun] Stopped.
pause
exit /b 0

:logs
call :ensure_docker
if errorlevel 1 exit /b 1
%COMPOSE% logs -f --tail=100
pause
exit /b 0

:status
call :ensure_docker
if errorlevel 1 exit /b 1
%COMPOSE% ps
pause
exit /b 0

:restart
call :ensure_docker
if errorlevel 1 exit /b 1
%COMPOSE% down
if errorlevel 1 goto :compose_failed
goto :start

:compose_failed
echo [Pojun] Docker Compose command failed. See the output above.
pause
exit /b 1

:help
echo Usage:
echo   start.bat            start containers and open %URL%
echo   start.bat status     show container status
echo   start.bat logs       follow logs
echo   start.bat stop       stop containers
echo   start.bat restart    restart containers
echo.
echo Requirements: Docker Desktop installed with the WSL2 backend.
echo On first run .env.example is copied to .env with a random DB password.
pause
exit /b 0

rem ---------------------------------------------------------------------------
rem Make sure the Docker CLI exists and the Docker Desktop engine is ready.
rem If Desktop is not running, try the standard install path and wait ~2 min.
rem ---------------------------------------------------------------------------
:ensure_docker
where docker >nul 2>&1
if errorlevel 1 (
	echo [Pojun] Docker CLI not found. Install Docker Desktop:
	echo   https://www.docker.com/products/docker-desktop/
	exit /b 1
)
docker info >nul 2>&1
if not errorlevel 1 exit /b 0

echo [Pojun] Docker Desktop is not running; trying to start it...
if exist "%ProgramFiles%\Docker\Docker\Docker Desktop.exe" (
	start "" "%ProgramFiles%\Docker\Docker\Docker Desktop.exe"
) else if exist "%ProgramFiles(x86)%\Docker\Docker\Docker Desktop.exe" (
	start "" "%ProgramFiles(x86)%\Docker\Docker\Docker Desktop.exe"
) else (
	echo [Pojun] Docker Desktop executable not found. Start Docker Desktop manually.
	exit /b 1
)
for /l %%i in (1,1,60) do (
	docker info >nul 2>&1
	if not errorlevel 1 exit /b 0
	ping -n 3 127.0.0.1 >nul
)
echo [Pojun] Docker Desktop did not become ready in time.
echo [Pojun] Start it manually, then re-run this script.
exit /b 1

rem ---------------------------------------------------------------------------
rem First run: copy .env.example to .env and replace the placeholder DB password
rem with a random value. Never overwrite an existing .env.
rem ---------------------------------------------------------------------------
:ensure_env
if exist ".env" exit /b 0
if not exist ".env.example" (
	echo [Pojun] .env.example not found. Are you in the repository root?
	exit /b 1
)
copy /y ".env.example" ".env" >nul
if errorlevel 1 (
	echo [Pojun] Failed to create .env.
	exit /b 1
)
powershell -NoProfile -ExecutionPolicy Bypass -Command "$p=(Resolve-Path '.env').Path; $s=Get-Content -Raw -LiteralPath $p; $pw=([guid]::NewGuid().ToString('N')+[guid]::NewGuid().ToString('N')).Substring(0,32); $s=$s -replace 'POSTGRES_PASSWORD=change-me-please',('POSTGRES_PASSWORD='+$pw); [IO.File]::WriteAllText($p,$s,(New-Object Text.UTF8Encoding($false)))"
if errorlevel 1 (
	echo [Pojun] Failed to generate .env. Edit .env and set POSTGRES_PASSWORD manually.
	exit /b 1
)
echo [Pojun] Created .env with a generated database password.
echo [Pojun] Edit .env if you want to set LLM keys or change ports.
exit /b 0

rem ---------------------------------------------------------------------------
rem Wait up to ~2 minutes for /api/health to return 200.
rem ---------------------------------------------------------------------------
:wait_health
for /l %%i in (1,1,60) do (
	powershell -NoProfile -Command "try { $r=Invoke-WebRequest -UseBasicParsing -Uri '%URL%/api/health' -TimeoutSec 2; if($r.StatusCode -eq 200){exit 0} } catch {}; exit 1" >nul 2>&1
	if not errorlevel 1 exit /b 0
	ping -n 3 127.0.0.1 >nul
)
exit /b 1
