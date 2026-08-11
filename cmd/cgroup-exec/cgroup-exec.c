/* Enter a preconfigured cgroup before executing the sandbox launcher. Descendants inherit it.
 * An empty --cgroup is the explicit Compose development mode. */
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

int main(int argc, char **argv) {
  if (argc < 5 || strcmp(argv[1], "--cgroup") || strcmp(argv[3], "--")) return 64;
  if (argv[2][0]) {
    char p[4096], pid[32];
    snprintf(p, sizeof p, "%s/cgroup.procs", argv[2]);
    int fd = open(p, O_WRONLY|O_CLOEXEC);
    if (fd < 0) { perror("open cgroup.procs"); return 126; }
    int n = snprintf(pid, sizeof pid, "%ld", (long)getpid());
    if (write(fd, pid, n) != n) { perror("write cgroup.procs"); return 126; }
    close(fd);
  }
  execvp(argv[4], argv + 4);
  perror("execvp"); return 127;
}
