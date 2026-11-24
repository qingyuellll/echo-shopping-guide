@echo off
chcp 65001 >nul
setlocal
cd /d "%~dp0frontend"
where node >nul 2>nul
if errorlevel 1 (echo 请先安装 Node.js 20.19+ 或 22.12+。 & pause & exit /b 1)
if not exist node_modules call npm ci
if errorlevel 1 exit /b 1
call npm run build:direct
if errorlevel 1 (pause & exit /b 1)
cd /d "%~dp0backend"
where go >nul 2>nul
if errorlevel 1 (echo 请安装 Go 1.24+ 后重新运行本脚本。 & pause & exit /b 1)
go build -trimpath -o guide.exe ./cmd/guide
if errorlevel 1 (pause & exit /b 1)
echo 构建完成。运行 start-guide.cmd 打开演出导购。
pause
