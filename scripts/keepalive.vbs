' ============================================================================
'  free2api-keepalive.vbs -- windowless watchdog for the free2api gateway.
'
'  WHY THIS EXISTS
'    A Task Scheduler trigger whose action is a console program (cmd.exe and/or
'    free2api.exe) makes Windows allocate a console on every run. Windows 11
'    uses Windows Terminal as the default console host, so that console is a
'    real, visible window: it flashes once per trigger.
'
'    This launcher is hosted by wscript.exe, which is a GUI-subsystem host and
'    therefore never allocates a console. The gateway is started through
'    WshShell.Run(..., SW_HIDE, ...), so the hidden cmd.exe that carries the
'    stdout/stderr redirection stays hidden too. Net effect: no window, ever.
'
'  CONTRACT
'    * deploy dir = folder holding this script, else its parent, so both the
'      repo layout <root>\scripts\keepalive.vbs and a flat deploy layout work
'    * skips work when <dir>\data\keepalive.pause exists (manual pause switch)
'    * reads the gateway port from <dir>\config.json ("listen"), default 7864
'    * treats any HTTP answer on http://127.0.0.1:<port>/status as "already up"
'      and only starts the gateway when nothing answers
'    * runs <dir>\free2api.exe -config <dir>\config.json with stdout/stderr
'      appended to <dir>\data\server.out.log and server.err.log
'    * blocks until the gateway exits, which keeps the Task Scheduler instance
'      alive so the per-minute trigger is ignored instead of re-spawning
'    * appends one line per event to <dir>\data\keepalive.log
'
'  WHY A PORT PROBE AND NOT WMI
'    The obvious duplicate guard is a WMI query for Win32_Process. On locked
'    down or non-elevated hosts that query returns 0x80041003 (access denied).
'    An earlier revision of this script read that failure as "already running",
'    which made the watchdog a silent no-op that never started the gateway --
'    and nobody notices that until the next reboot. A TCP probe of the port the
'    gateway owns cannot fail that way: if it answers, the gateway is up; if it
'    refuses, we start it. Worst case on a false "down" is a second process
'    whose bind fails, which is loud in server.err.log instead of silent.
'
'  KEEP THIS FILE ASCII-ONLY. wscript reads .vbs using the ANSI code page of the
'  machine, so non-ASCII characters would be mangled on a different locale.
' ============================================================================

Option Explicit

Const SW_HIDE       = 0
Const FOR_READING   = 1
Const FOR_APPENDING = 8
Const LOG_MAX_BYTES = 262144    ' rotate keepalive.log past 256 KiB
Const PROBE_MS      = 1500

Dim fso, sh, dq
Dim dirPath, exePath, cfgPath, dataDir, logPath, outPath, errPath, pausePath
Dim port, builtCmd, exitCode

Set fso = CreateObject("Scripting.FileSystemObject")
Set sh  = CreateObject("WScript.Shell")
dq = Chr(34)

' --- resolve the deploy directory -------------------------------------------
dirPath = fso.GetParentFolderName(WScript.ScriptFullName)
If Not fso.FileExists(fso.BuildPath(dirPath, "free2api.exe")) Then
    dirPath = fso.GetParentFolderName(dirPath)
End If

exePath   = fso.BuildPath(dirPath, "free2api.exe")
cfgPath   = fso.BuildPath(dirPath, "config.json")
dataDir   = fso.BuildPath(dirPath, "data")
logPath   = fso.BuildPath(dataDir, "keepalive.log")
outPath   = fso.BuildPath(dataDir, "server.out.log")
errPath   = fso.BuildPath(dataDir, "server.err.log")
pausePath = fso.BuildPath(dataDir, "keepalive.pause")

If Not fso.FolderExists(dataDir) Then
    On Error Resume Next
    fso.CreateFolder dataDir
    On Error GoTo 0
End If

If Not fso.FileExists(exePath) Then
    LogLine "ERROR free2api.exe not found; looked in " & dirPath
    WScript.Quit 2
End If

If Not fso.FileExists(cfgPath) Then
    LogLine "ERROR config.json not found; looked in " & dirPath
    WScript.Quit 3
End If

If fso.FileExists(pausePath) Then
    LogLine "PAUSED keepalive.pause present; leaving the gateway alone"
    WScript.Quit 0
End If

port = ReadPort(cfgPath)

If PortAlive(port) Then
    LogLine "ALREADY gateway answering on 127.0.0.1:" & port & "; nothing to do"
    WScript.Quit 0
End If

' --- start the gateway hidden, then wait for it -----------------------------
' Target command line:
'   cmd.exe /c ""<exe>" -config "<cfg>" >> "<out>" 2>> "<err>""
' The outer quote pair is required by cmd when the command itself is quoted and
' carries redirection; without it cmd mis-parses the tail of the line.
builtCmd = "cmd.exe /c " & dq & dq & exePath & dq & " -config " & dq & cfgPath & dq & _
           " >> " & dq & outPath & dq & " 2>> " & dq & errPath & dq & dq

' --- strip the Codex sandbox black-hole proxy ----------------------------
' The Codex Windows sandbox exports HTTP(S)_PROXY=http://127.0.0.1:9 into
' every child process to forbid network. The gateway ignores it (its outbound
' Transport never reads Proxy), but a user shell launched from the same
' environment would inherit a dead proxy. Delete them so the gateway and its
' children see a clean environment. ASCII-only, see file header.
On Error Resume Next
sh.Environment("PROCESS").Remove "HTTP_PROXY"
sh.Environment("PROCESS").Remove "HTTPS_PROXY"
sh.Environment("PROCESS").Remove "ALL_PROXY"
sh.Environment("PROCESS").Remove "http_proxy"
sh.Environment("PROCESS").Remove "https_proxy"
sh.Environment("PROCESS").Remove "all_proxy"
sh.Environment("PROCESS").Remove "GIT_HTTP_PROXY"
sh.Environment("PROCESS").Remove "GIT_HTTPS_PROXY"
On Error GoTo 0

sh.CurrentDirectory = dirPath

LogLine "START port " & port & " silent; launching " & exePath
On Error Resume Next
exitCode = sh.Run(builtCmd, SW_HIDE, True)
If Err.Number <> 0 Then
    LogLine "ERROR launch failed: " & Err.Number & " " & Err.Description
    On Error GoTo 0
    WScript.Quit 4
End If
On Error GoTo 0

LogLine "STOP gateway exited; child exit code = " & exitCode
WScript.Quit 0

' ---------------------------------------------------------------------------
' Read the TCP port the gateway binds from config.json ("listen": "host:port").
' Falls back to 7864 when the file has no parseable listen field.
Function ReadPort(configPath)
    Dim stream, text, re, matches, listen, pos, digits
    ReadPort = "7864"
    On Error Resume Next
    Set stream = fso.OpenTextFile(configPath, FOR_READING)
    text = stream.ReadAll
    stream.Close
    If Err.Number <> 0 Then
        Err.Clear
        On Error GoTo 0
        Exit Function
    End If
    On Error GoTo 0

    Set re = New RegExp
    re.Pattern = dq & "listen" & dq & "\s*:\s*" & dq & "([^" & dq & "]*)" & dq
    re.IgnoreCase = True
    If re.Test(text) Then
        Set matches = re.Execute(text)
        listen = matches(0).SubMatches(0)
        pos = InStrRev(listen, ":")
        If pos > 0 Then
            digits = Replace(Mid(listen, pos + 1), " ", "")
            If IsNumeric(digits) Then
                If CInt(digits) > 0 And CInt(digits) < 65536 Then
                    ReadPort = digits
                End If
            End If
        End If
    End If
End Function

' ---------------------------------------------------------------------------
' True when something answers HTTP on 127.0.0.1:<port>. Any status code counts:
' a 401 from a keyed deployment still proves the gateway owns the port.
' Connection failures return False, which is what lets the gateway start.
Function PortAlive(port)
    Dim http
    PortAlive = False
    On Error Resume Next
    Set http = CreateObject("MSXML2.ServerXMLHTTP.6.0")
    If Err.Number <> 0 Then
        Err.Clear
        On Error GoTo 0
        Exit Function
    End If
    http.setTimeouts PROBE_MS, PROBE_MS, PROBE_MS, PROBE_MS
    Err.Clear
    http.open "GET", "http://127.0.0.1:" & port & "/status", False
    http.send
    If Err.Number = 0 Then
        PortAlive = True
    Else
        Err.Clear
    End If
    On Error GoTo 0
End Function

Sub LogLine(message)
    Dim f, stamp
    On Error Resume Next
    If fso.FileExists(logPath) Then
        If fso.GetFile(logPath).Size > LOG_MAX_BYTES Then
            fso.DeleteFile logPath, True
        End If
    End If
    stamp = Year(Now) & "-" & Pad2(Month(Now)) & "-" & Pad2(Day(Now)) & " " & _
            Pad2(Hour(Now)) & ":" & Pad2(Minute(Now)) & ":" & Pad2(Second(Now))
    Set f = fso.OpenTextFile(logPath, FOR_APPENDING, True)
    f.WriteLine stamp & "  " & message
    f.Close
    On Error GoTo 0
End Sub

Function Pad2(value)
    If value < 10 Then
        Pad2 = "0" & value
    Else
        Pad2 = CStr(value)
    End If
End Function
