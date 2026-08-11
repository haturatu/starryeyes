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
#include <sys/syscall.h>
#include <unistd.h>

#define REQUIRED_LANDLOCK_ABI 4
/* Debian 12 ships Linux UAPI headers from before ABI 3/4.  The running kernel
 * is queried at runtime, but use this ABI-stable extension layout to compile
 * against those older headers. */
#ifndef LANDLOCK_ACCESS_FS_TRUNCATE
#define LANDLOCK_ACCESS_FS_TRUNCATE (1ULL << 14)
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
static void landlock(const char *input, const char *output) {
  int abi=ll_create(NULL,0,LANDLOCK_CREATE_RULESET_VERSION);
  if(abi<REQUIRED_LANDLOCK_ABI) { fprintf(stderr,"Landlock ABI %d; need >= %d\n",abi,REQUIRED_LANDLOCK_ABI); exit(126); }
  uint64_t ro=LANDLOCK_ACCESS_FS_EXECUTE|LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR;
  uint64_t rw=LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR|LANDLOCK_ACCESS_FS_WRITE_FILE|LANDLOCK_ACCESS_FS_TRUNCATE|LANDLOCK_ACCESS_FS_REMOVE_FILE|LANDLOCK_ACCESS_FS_REMOVE_DIR|LANDLOCK_ACCESS_FS_MAKE_REG|LANDLOCK_ACCESS_FS_MAKE_DIR|LANDLOCK_ACCESS_FS_REFER;
  uint64_t handled=ro|LANDLOCK_ACCESS_FS_WRITE_FILE|LANDLOCK_ACCESS_FS_TRUNCATE|LANDLOCK_ACCESS_FS_REMOVE_FILE|LANDLOCK_ACCESS_FS_REMOVE_DIR|LANDLOCK_ACCESS_FS_MAKE_REG|LANDLOCK_ACCESS_FS_MAKE_DIR|LANDLOCK_ACCESS_FS_MAKE_CHAR|LANDLOCK_ACCESS_FS_MAKE_BLOCK|LANDLOCK_ACCESS_FS_MAKE_SYM|LANDLOCK_ACCESS_FS_MAKE_FIFO|LANDLOCK_ACCESS_FS_MAKE_SOCK|LANDLOCK_ACCESS_FS_REFER;
  struct ruleset_attr_v4 r={.handled_access_fs=handled,.handled_access_net=LANDLOCK_ACCESS_NET_BIND_TCP|LANDLOCK_ACCESS_NET_CONNECT_TCP};
  int fd=ll_create(&r,sizeof(r),0); if(fd<0) die("landlock_create_ruleset");
  allow_path(fd,"/usr",ro); allow_path(fd,"/lib",ro); allow_path(fd,"/lib64",ro);
  allow_path(fd,"/etc",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR);
  allow_path(fd,"/proc",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR);
  allow_path(fd,"/dev/null",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_WRITE_FILE);
  allow_path(fd,"/dev/urandom",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_WRITE_FILE);
  allow_path(fd,input,LANDLOCK_ACCESS_FS_READ_FILE);
  allow_path(fd,"/tmp",rw);
  if(output) allow_path(fd,output,rw);
  if(prctl(PR_SET_NO_NEW_PRIVS,1,0,0,0)<0) die("PR_SET_NO_NEW_PRIVS");
  if(ll_restrict(fd)<0) die("landlock_restrict_self");
  close(fd);
}
static void deny(scmp_filter_ctx c,int nr){if(seccomp_rule_add(c,SCMP_ACT_ERRNO(EPERM),nr,0)<0)exit(126);}
static void seccomp_cpu(void){scmp_filter_ctx c=seccomp_init(SCMP_ACT_ALLOW);if(!c)exit(126);deny(c,SCMP_SYS(socket));deny(c,SCMP_SYS(socketpair));deny(c,SCMP_SYS(connect));deny(c,SCMP_SYS(bind));deny(c,SCMP_SYS(listen));deny(c,SCMP_SYS(accept));deny(c,SCMP_SYS(accept4));deny(c,SCMP_SYS(sendto));deny(c,SCMP_SYS(recvfrom));deny(c,SCMP_SYS(unshare));deny(c,SCMP_SYS(setns));deny(c,SCMP_SYS(mount));deny(c,SCMP_SYS(umount2));deny(c,SCMP_SYS(pivot_root));deny(c,SCMP_SYS(ptrace));deny(c,SCMP_SYS(bpf));deny(c,SCMP_SYS(keyctl));deny(c,SCMP_SYS(kexec_load));if(seccomp_load(c)<0)die("seccomp_load");seccomp_release(c);}
int main(int argc,char **argv){
  if(argc==2&&!strcmp(argv[1],"--check")){int a=ll_create(NULL,0,LANDLOCK_CREATE_RULESET_VERSION);return a>=REQUIRED_LANDLOCK_ABI?0:1;}
  if(argc<7||strcmp(argv[1],"--profile")||strcmp(argv[2],"cpu")||strcmp(argv[3],"--input"))return 64;
  const char *input=argv[4],*output=NULL;int i=5;
  if(i+1<argc&&!strcmp(argv[i],"--output")){output=argv[i+1];i+=2;}
  if(i>=argc||strcmp(argv[i],"--")||i+1>=argc)return 64;
  landlock(input,output);seccomp_cpu();execvp(argv[i+1],argv+i+1);die("execvp");
}
