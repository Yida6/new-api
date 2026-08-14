@echo off
setlocal EnableExtensions EnableDelayedExpansion

set "PROJECT_DIR=%~dp0"
set "COMPOSE_FILE=%PROJECT_DIR%docker-compose.dev.yml"
set "DOCKER_DESKTOP=C:\Program Files\Docker\Docker\Docker Desktop.exe"

if /I "%~1"=="frontend" goto frontend

set "BUILD_BACKEND=0"
if /I "%~1"=="rebuild" set "BUILD_BACKEND=1"

echo [1/4] Checking development tools...
where docker >nul 2>&1
if errorlevel 1 (
  echo [ERROR] Docker was not found. Install Docker Desktop first.
  goto failed
)

where bun >nul 2>&1
if not errorlevel 1 (
  for /f "delims=" %%I in ('where bun') do if not defined BUN_EXE set "BUN_EXE=%%I"
) else if exist "%USERPROFILE%\.bun\bin\bun.exe" (
  set "BUN_EXE=%USERPROFILE%\.bun\bin\bun.exe"
) else (
  echo [ERROR] Bun was not found. Reinstall Bun or reopen the terminal.
  goto failed
)

echo [2/4] Checking Docker engine...
docker info >nul 2>&1
if errorlevel 1 (
  if not exist "%DOCKER_DESKTOP%" (
    echo [ERROR] Docker Desktop is not running and its executable was not found.
    goto failed
  )

  echo Starting Docker Desktop. Please wait...
  start "" "%DOCKER_DESKTOP%"
  for /L %%I in (1,1,30) do (
    docker info >nul 2>&1
    if not errorlevel 1 goto docker_ready
    ping 127.0.0.1 -n 3 >nul
  )

  echo [ERROR] Docker did not become ready within 60 seconds.
  goto failed
)

:docker_ready
docker image inspect new-api-dev:local >nul 2>&1
if errorlevel 1 set "BUILD_BACKEND=1"

if "%BUILD_BACKEND%"=="1" (
  echo [3/4] Rebuilding and starting PostgreSQL, Redis, and backend...
  docker compose -f "%COMPOSE_FILE%" up -d --build
) else (
  echo [3/4] Starting PostgreSQL, Redis, and backend without rebuilding...
  docker compose -f "%COMPOSE_FILE%" up -d --no-build
)
if errorlevel 1 (
  echo [ERROR] Backend startup failed. Review the Docker output above.
  goto failed
)

echo Waiting for backend to become ready...
for /L %%I in (1,1,30) do (
  curl.exe --silent --fail --output NUL "http://localhost:3000/api/status" >nul 2>&1
  if not errorlevel 1 goto backend_ready
  ping 127.0.0.1 -n 3 >nul
)
echo [ERROR] Backend did not become ready within 60 seconds.
goto failed

:backend_ready
echo [4/4] Starting frontend development server...
powershell -NoProfile -Command "if (Get-NetTCPConnection -LocalPort 5173 -State Listen -ErrorAction SilentlyContinue) { exit 0 } else { exit 1 }"
if errorlevel 1 (
  start "New API Frontend" cmd /k call "%~f0" frontend
) else (
  echo Port 5173 is already listening. Skipping duplicate frontend startup.
)

echo Waiting for frontend to become ready...
for /L %%I in (1,1,60) do (
  curl.exe --silent --fail --output NUL "http://localhost:5173/" >nul 2>&1
  if not errorlevel 1 goto frontend_ready
  ping 127.0.0.1 -n 3 >nul
)
echo [WARNING] Frontend did not become ready within 120 seconds.

:frontend_ready
start "" "http://localhost:5173"

echo.
echo Development environment is ready:
echo   Frontend: http://localhost:5173
echo   Backend:  http://localhost:3000
echo.
echo Backend Go changes reload automatically after you save the file.
echo Use start-dev.bat rebuild only after changing Dockerfile.dev.
echo Stop backend services with: docker compose -f docker-compose.dev.yml down
exit /b 0

:frontend
cd /d "%PROJECT_DIR%web"
if not defined BUN_EXE (
  where bun >nul 2>&1
  if not errorlevel 1 (
    for /f "delims=" %%I in ('where bun') do if not defined BUN_EXE set "BUN_EXE=%%I"
  ) else if exist "%USERPROFILE%\.bun\bin\bun.exe" (
    set "BUN_EXE=%USERPROFILE%\.bun\bin\bun.exe"
  )
)
if not defined BUN_EXE (
  echo [ERROR] Bun was not found.
  exit /b 1
)

set "LOCK_HASH="
for /f "usebackq delims=" %%H in (`powershell -NoProfile -Command "(Get-FileHash -Algorithm SHA256 'bun.lock').Hash"`) do set "LOCK_HASH=%%H"
set "INSTALLED_HASH="
if exist "node_modules\.new-api-bun-lock.sha256" set /p INSTALLED_HASH=<"node_modules\.new-api-bun-lock.sha256"

if not exist "node_modules\.bin\rsbuild.exe" goto install_frontend
if /I not "!LOCK_HASH!"=="!INSTALLED_HASH!" goto install_frontend
echo Frontend dependencies are up to date. Skipping bun install.
goto run_frontend

:install_frontend
echo Installing frontend dependencies...
"%BUN_EXE%" install --frozen-lockfile
if errorlevel 1 (
  echo [ERROR] Frontend dependency installation failed.
  exit /b 1
)
>"node_modules\.new-api-bun-lock.sha256" echo !LOCK_HASH!

:run_frontend
echo Starting frontend development server...
"%BUN_EXE%" run dev -- --host 0.0.0.0 --port 5173
exit /b %errorlevel%

:failed
echo.
pause
exit /b 1
