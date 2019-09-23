package metricstore

const (
	INIT                         = "init"
	INIT_CreateDomainNotifier    = "init/createDomainNotifier"
	INIT_CreateLibvirtConnection = "init/createLibvirtConnection"
	INIT_CreateNotifier          = "init/createNotifier"
	INIT_CreateServer            = "init/createServer"
	INIT_WaitForDomainUUID       = "init/waitForDomainUUID"

	INIT_Libvirt_LookUpDomainByName     = "init/libvirt/lookUpDomainByName"
	INIT_Libvirt_PreStartHook           = "init/libvirt/preStartHook"
	INIT_Libvirt_SetDomainSpecWithHooks = "init/libvirt/setDomainSpecWithHooks"
	INIT_Libvirt_StartDomain            = "init/libvirt/startDomain"
)

const (
	DESTROY             = "destroy"
	DESTROY_StopMonitor = "destroy/stopMonitor"
	DESTROY_StopServer  = "destroy/stopServer"

	DESTROY_Libvirt_shutDownFlags = "destroy/libivrt/shutdownFlags"
	DESTROY_Libvirt_destroyFlags  = "destroy/libvirt/destroyFlags"
	DESTROY_Libvirt_undefineFlags = "destroy/libvirt/undefineFlags"
)
