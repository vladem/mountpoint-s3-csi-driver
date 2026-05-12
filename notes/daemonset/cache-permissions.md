# Child process isolation + cache directory permissions

- Mounter pod runs as root with minimal caps: SETUID, SETGID, KILL, CHOWN, DAC_OVERRIDE. CHOWN and DAC_OVERRIDE are only needed for cache dir lifecycle (create+chown before exec, cleanup after exit). Kept in the mounter for simplicity, may revisit.
- Each Mountpoint child gets a unique UID (2000+). The UID 0→N transition clears all caps [automatically](https://man7.org/linux/man-pages/man7/capabilities.7.html).
- Cache dirs: `/comm/cache/<mountId>` created by parent (root), chowned to child UID, mode 0700.
- Parent dir `/comm/cache` is 0777 (same as `/comm` emptyDir default) so children can traverse.
- Children cannot access each other's cache dirs. Parent cleans up on child exit.

## Verification

```
$ ks exec -it s3-csi-mounter-hllvz -c mounter -- ls -la /comm/cache
total 16
drwxr-xr-x. 7 root root 16384 May 12 13:05 .
drwxrwxrwx. 3 root root    37 May 12 13:05 ..
drwx------. 3 2000 2000    30 May 12 13:05 1f18b108-7ae0-47d8-bdb3-883f07169cc7-s3-csi-driver-volume
drwx------. 3 2002 2002    30 May 12 13:05 37ae87f0-6d78-40ef-91fa-b7728e864ca0-s3-csi-driver-volume
drwx------. 3 2003 2003    30 May 12 13:05 6f8260e9-de1b-472b-b9a8-e25e9f3026b0-s3-csi-driver-volume
drwx------. 3 2001 2001    30 May 12 13:05 cc726bff-92ed-445d-a90e-b5c6ae34fc04-s3-csi-driver-volume
drwx------. 3 2004 2004    30 May 12 13:05 d61f76f5-ea02-4eee-9611-4b87dd50d07d-s3-csi-driver-volume

$ ks exec -it s3-csi-mounter-hllvz -c mounter -- sh -c 'for pid in $(ls /proc | grep -E "^[0-9]+$"); do echo "=== PID $pid ==="; grep -E "^(Name|PPid|Uid|Gid|Cap)" /proc/$pid/status 2>/dev/null; echo; done'
=== PID 1 ===
Name:   aws-s3-csi-daem
PPid:   0
Uid:    0       0       0       0
Gid:    0       0       0       0
CapInh: 0000000000000000
CapPrm: 00000000000000e3
CapEff: 00000000000000e3
CapBnd: 00000000000000e3
CapAmb: 0000000000000000

=== PID 14 ===
Name:   mount-s3
PPid:   1
Uid:    2000    2000    2000    2000
Gid:    2000    2000    2000    2000
CapInh: 0000000000000000
CapPrm: 0000000000000000
CapEff: 0000000000000000
CapBnd: 00000000000000e3
CapAmb: 0000000000000000

<...>

=== PID 79 ===
Name:   mount-s3
PPid:   1
Uid:    2004    2004    2004    2004
Gid:    2004    2004    2004    2004
CapInh: 0000000000000000
CapPrm: 0000000000000000
CapEff: 0000000000000000
CapBnd: 00000000000000e3
CapAmb: 0000000000000000
```
