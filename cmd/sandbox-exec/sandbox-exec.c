/* Landlock + seccomp launcher.  It must run immediately before ffprobe/ffmpeg:
 * Landlock restrictions are inherited across execve and cannot be relaxed. */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/landlock.h>
#include <seccomp.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <unistd.h>

#define REQUIRED_LANDLOCK_ABI 4
/* Debian 12 ships Linux UAPI headers from before ABI 3/4.  The running kernel
 * is queried at runtime, but use this ABI-stable extension layout to compile
 * against those older headers. */
#ifndef LANDLOCK_ACCESS_FS_TRUNCATE
#define LANDLOCK_ACCESS_FS_TRUNCATE (1ULL << 14)
#endif
#ifndef LANDLOCK_ACCESS_FS_IOCTL_DEV
#define LANDLOCK_ACCESS_FS_IOCTL_DEV (1ULL << 15)
#endif
#ifndef LANDLOCK_ACCESS_NET_BIND_TCP
#define LANDLOCK_ACCESS_NET_BIND_TCP (1ULL << 0)
#define LANDLOCK_ACCESS_NET_CONNECT_TCP (1ULL << 1)
#endif
struct ruleset_attr_v4 { uint64_t handled_access_fs; uint64_t handled_access_net; uint64_t scoped; };
static int ll_create(const void *a, size_t n, uint32_t f) { return syscall(SYS_landlock_create_ruleset,a,n,f); }
static int ll_add(int fd, const struct landlock_path_beneath_attr *a) { return syscall(SYS_landlock_add_rule,fd,LANDLOCK_RULE_PATH_BENEATH,a,0); }
static int ll_restrict(int fd) { return syscall(SYS_landlock_restrict_self,fd,0); }
static void die(const char *what) { perror(what); exit(126); }
static void allow_path(int ruleset, const char *path, uint64_t rights) {
  int fd=open(path,O_PATH|O_CLOEXEC); if(fd<0) die(path);
  struct landlock_path_beneath_attr a={.allowed_access=rights,.parent_fd=fd};
  if(ll_add(ruleset,&a)<0) die("landlock_add_rule");
  close(fd);
}
static void allow_optional_path(int ruleset, const char *path, uint64_t rights) {
  int fd=open(path,O_PATH|O_CLOEXEC);
  if(fd<0) {
    if(errno==ENOENT || errno==ENOTDIR) return;
    die(path);
  }
  struct landlock_path_beneath_attr a={.allowed_access=rights,.parent_fd=fd};
  if(ll_add(ruleset,&a)<0) die("landlock_add_rule");
  close(fd);
}
/* Mesa/libdrm resolves DRM device metadata through these read-only sysfs trees. */
static void allow_gpu_sysfs(int ruleset) {
  uint64_t ro=LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR;
  allow_path(ruleset,"/sys/dev",ro);
  /* NVIDIA exposes capability and misc-device metadata below /sys/class,
   * while Mesa/libdrm uses /sys/class/drm. */
  allow_path(ruleset,"/sys/class",ro);
  allow_path(ruleset,"/sys/bus",ro);
  allow_path(ruleset,"/sys/devices",ro);
  allow_optional_path(ruleset,"/sys/module",ro);
}
static void allow_nvidia_proc(int ruleset) {
  uint64_t ro=LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR;
  /* NVIDIA userspace uses this tree for driver and capability discovery. */
  allow_optional_path(ruleset,"/proc/driver/nvidia",ro);
  /* CUDA/NVML may inspect the loaded module and kernel version while
   * validating the driver ABI.  These files are read-only and only exposed
   * to a worker that already has explicit GPU device access. */
  allow_optional_path(ruleset,"/proc/modules",LANDLOCK_ACCESS_FS_READ_FILE);
  allow_optional_path(ruleset,"/proc/version",LANDLOCK_ACCESS_FS_READ_FILE);
  allow_optional_path(ruleset,"/proc/devices",LANDLOCK_ACCESS_FS_READ_FILE);
  allow_optional_path(ruleset,"/proc/misc",LANDLOCK_ACCESS_FS_READ_FILE);
  allow_optional_path(ruleset,"/proc/filesystems",LANDLOCK_ACCESS_FS_READ_FILE);
}
static void allow_gpu_thread_names(int ruleset) {
  uint64_t rw=LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR|LANDLOCK_ACCESS_FS_WRITE_FILE|LANDLOCK_ACCESS_FS_TRUNCATE|LANDLOCK_ACCESS_FS_MAKE_REG;
  /* CUDA/NVIDIA worker threads set their names through procfs.  Keep this
   * write permission limited to this process's own task directories. */
  allow_optional_path(ruleset,"/proc/self/task",rw);
  allow_optional_path(ruleset,"/proc/thread-self",rw);
}
static void allow_nvidia_runtime_socket(int ruleset) {
  uint64_t dir=LANDLOCK_ACCESS_FS_READ_DIR;
  uint64_t socket=LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_WRITE_FILE;
  /* The NVIDIA container runtime may expose this AF_UNIX endpoint for
   * persistence/NVML operations. Keep the allowlist scoped to this socket. */
  allow_optional_path(ruleset,"/run/nvidia-persistenced",dir);
  allow_optional_path(ruleset,"/run/nvidia-persistenced/socket",socket);
}
static void landlock(const char *input, const char *output, const char **gpu_devices, int gpu_count) {
  int abi=ll_create(NULL,0,LANDLOCK_CREATE_RULESET_VERSION);
  if(abi<REQUIRED_LANDLOCK_ABI) { fprintf(stderr,"Landlock ABI %d; need >= %d\n",abi,REQUIRED_LANDLOCK_ABI); exit(126); }
  uint64_t ro=LANDLOCK_ACCESS_FS_EXECUTE|LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR;
  uint64_t rw=LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR|LANDLOCK_ACCESS_FS_WRITE_FILE|LANDLOCK_ACCESS_FS_TRUNCATE|LANDLOCK_ACCESS_FS_REMOVE_FILE|LANDLOCK_ACCESS_FS_MAKE_REG|LANDLOCK_ACCESS_FS_MAKE_DIR;
  uint64_t handled=ro|LANDLOCK_ACCESS_FS_WRITE_FILE|LANDLOCK_ACCESS_FS_TRUNCATE|LANDLOCK_ACCESS_FS_REMOVE_FILE|LANDLOCK_ACCESS_FS_REMOVE_DIR|LANDLOCK_ACCESS_FS_MAKE_REG|LANDLOCK_ACCESS_FS_MAKE_DIR|LANDLOCK_ACCESS_FS_MAKE_CHAR|LANDLOCK_ACCESS_FS_MAKE_BLOCK|LANDLOCK_ACCESS_FS_MAKE_SYM|LANDLOCK_ACCESS_FS_MAKE_FIFO|LANDLOCK_ACCESS_FS_MAKE_SOCK|LANDLOCK_ACCESS_FS_REFER;
  uint64_t gpu_rights=LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_WRITE_FILE;
  /* ABI 5 introduced device ioctl restriction.  Explicitly allow ioctl on
   * the selected GPU nodes, while keeping it unavailable on every other
   * device.  On ABI 4 kernels the bit must not be included in the ruleset. */
  if(abi>=5) {
    handled|=LANDLOCK_ACCESS_FS_IOCTL_DEV;
    gpu_rights|=LANDLOCK_ACCESS_FS_IOCTL_DEV;
  }
  struct ruleset_attr_v4 r={.handled_access_fs=handled,.handled_access_net=LANDLOCK_ACCESS_NET_BIND_TCP|LANDLOCK_ACCESS_NET_CONNECT_TCP};
  int fd=ll_create(&r,sizeof(r),0); if(fd<0) die("landlock_create_ruleset");
  allow_path(fd,"/usr",ro); allow_path(fd,"/lib",ro); allow_path(fd,"/lib64",ro);
  allow_path(fd,"/etc",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR);
  allow_path(fd,"/proc/self",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR);
  allow_path(fd,"/proc/thread-self",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR);
  allow_path(fd,"/proc/cpuinfo",LANDLOCK_ACCESS_FS_READ_FILE);
  allow_path(fd,"/proc/meminfo",LANDLOCK_ACCESS_FS_READ_FILE);
  allow_path(fd,"/proc/sys",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR);
  allow_path(fd,"/dev/null",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_WRITE_FILE);
  allow_path(fd,"/dev/urandom",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_WRITE_FILE);
  for(int i=0;i<gpu_count;i++) allow_path(fd,gpu_devices[i],gpu_rights);
  if(gpu_count>0) {
    /* libdrm opens this directory when a DRM node is present. NVIDIA-only
     * containers may not expose /dev/dri, so it is optional here; a VA-API
     * render node passed above remains mandatory. */
    allow_optional_path(fd,"/dev/dri",LANDLOCK_ACCESS_FS_READ_DIR);
    /* libcuda may resolve NVIDIA nodes through udev's /dev/char symlinks
     * (for example /dev/char/195:255 -> ../nvidiactl).  The final target is
     * still restricted to the explicitly selected GPU device nodes above. */
    allow_optional_path(fd,"/dev/char",LANDLOCK_ACCESS_FS_READ_DIR);
    allow_optional_path(fd,"/dev/nvidia-caps",LANDLOCK_ACCESS_FS_READ_DIR);
    /* CUDA may use POSIX shared memory for local runtime IPC.  The container
     * has its own /dev/shm namespace; grant it only to GPU workers. */
    allow_optional_path(fd,"/dev/shm",rw);
    allow_gpu_sysfs(fd);
    allow_nvidia_proc(fd);
    allow_gpu_thread_names(fd);
    allow_nvidia_runtime_socket(fd);
  }
  allow_path(fd,input,LANDLOCK_ACCESS_FS_READ_FILE);
  allow_path(fd,"/tmp",rw);
  if(output) allow_path(fd,output,rw);
  if(prctl(PR_SET_NO_NEW_PRIVS,1,0,0,0)<0) die("PR_SET_NO_NEW_PRIVS");
  if(ll_restrict(fd)<0) die("landlock_restrict_self");
  close(fd);
}
static void deny(scmp_filter_ctx c,int nr){if(seccomp_rule_add(c,SCMP_ACT_ERRNO(EPERM),nr,0)<0)exit(126);}
static void deny_socket_family(scmp_filter_ctx c, int family) {
  if(seccomp_rule_add(c,SCMP_ACT_ERRNO(EPERM),SCMP_SYS(socket),1,SCMP_CMP(0,SCMP_CMP_EQ,family))<0) exit(126);
}
static void seccomp_cpu(void) {
  scmp_filter_ctx c=seccomp_init(SCMP_ACT_ALLOW); if(!c) exit(126);

  /* Block creation of network-capable sockets while preserving AF_UNIX,
   * socketpair(), and local IPC used by GPU/runtime libraries.  Landlock
   * separately denies TCP bind/connect, and FFmpeg restricts protocols to
   * file and pipe.  AF_NETLINK is deliberately left available for libdrm
   * and udev-style device discovery. */
  deny_socket_family(c,AF_INET);
  deny_socket_family(c,AF_INET6);
  deny_socket_family(c,AF_PACKET);
#ifdef AF_VSOCK
  deny_socket_family(c,AF_VSOCK);
#endif
#ifdef AF_ALG
  deny_socket_family(c,AF_ALG);
#endif

  /* Namespace / host manipulation. */
  deny(c,SCMP_SYS(unshare));
  deny(c,SCMP_SYS(setns));
  deny(c,SCMP_SYS(mount));
  deny(c,SCMP_SYS(umount2));
  deny(c,SCMP_SYS(pivot_root));

  /* Kernel attack / inspection surfaces. */
  deny(c,SCMP_SYS(ptrace));
  deny(c,SCMP_SYS(bpf));
  deny(c,SCMP_SYS(keyctl));
  deny(c,SCMP_SYS(kexec_load));

  if(seccomp_load(c)<0) die("seccomp_load");
  seccomp_release(c);
}
static int numeric_suffix(const char *path, const char *prefix) {
  size_t n=strlen(prefix); if(strncmp(path,prefix,n)||!path[n]) return 0;
  for(const char *p=path+n;*p;p++) if(*p<'0'||*p>'9') return 0;
  return 1;
}
static int valid_gpu_device(const char *path) {
  return numeric_suffix(path,"/dev/dri/renderD") || numeric_suffix(path,"/dev/nvidia") || numeric_suffix(path,"/dev/nvidia-caps/nvidia-cap") || !strcmp(path,"/dev/nvidiactl") || !strcmp(path,"/dev/nvidia-modeset") || !strcmp(path,"/dev/nvidia-uvm") || !strcmp(path,"/dev/nvidia-uvm-tools");
}
int main(int argc,char **argv){
  if(argc==2&&!strcmp(argv[1],"--check")){int a=ll_create(NULL,0,LANDLOCK_CREATE_RULESET_VERSION);return a>=REQUIRED_LANDLOCK_ABI?0:1;}
  if(argc<7||strcmp(argv[1],"--profile")||strcmp(argv[2],"cpu")||strcmp(argv[3],"--input"))return 64;
  const char *input=argv[4],*output=NULL;const char **gpu_devices=calloc((size_t)argc,sizeof(*gpu_devices));int gpu_count=0,i=5;
  if(!gpu_devices) return 126;
  if(i+1<argc&&!strcmp(argv[i],"--output")){output=argv[i+1];i+=2;}
  while(i+1<argc&&!strcmp(argv[i],"--gpu-device")){
    if(!valid_gpu_device(argv[i+1])) return 64;
    gpu_devices[gpu_count++]=argv[i+1];i+=2;
  }
  if(i>=argc||strcmp(argv[i],"--")||i+1>=argc)return 64;
  landlock(input,output,gpu_devices,gpu_count);
#ifndef SANDBOX_EXEC_LANDLOCK_ONLY
  seccomp_cpu();
#endif
  execvp(argv[i+1],argv+i+1);die("execvp");
}
