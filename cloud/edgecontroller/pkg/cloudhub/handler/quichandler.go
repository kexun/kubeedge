package handler

import (
	"fmt"
	"github.com/kubeedge/viaduct/pkg/conn"
	"github.com/kubeedge/viaduct/pkg/mux"
	emodel "github.com/kubeedge/kubeedge/cloud/edgecontroller/pkg/cloudhub/common/model"
	hubio "github.com/kubeedge/kubeedge/cloud/edgecontroller/pkg/cloudhub/common/io"
)

var QuicHandler *QuicHandle

//QuicHandle access handler
type QuicHandle struct {
	EventHandler *EventHandle
	NodeLimit   int
}

func (qh *QuicHandle) HandleServer(container *mux.MessageContainer, writer mux.ResponseWriter) {
	fmt.Printf("kexun QuicHandle HandleServer\n")
	nodeId := container.Header.Get("node_id")
	projectId := container.Header.Get("project_id")
	fmt.Printf("kexun HandleServer projectId %s, nodeId: %s header: %v\n",projectId, nodeId, container.Header )
	err := qh.EventHandler.Pub2Controller(&emodel.HubInfo{projectId, nodeId}, container.Message)
	if err != nil {
		// if err, we should stop node, write data to edgehub, stop nodify
	}
}

func (qh *QuicHandle) OnRegister(connection conn.Connection) {
	nodeId := connection.ConnectionState().Headers.Get("node_id")
	projectId := connection.ConnectionState().Headers.Get("project_id")
	fmt.Printf("kexun OnRegister projectId %s, nodeId: %s header: %v\n",projectId, nodeId, connection.ConnectionState() )

	quicio := &hubio.JsonQuicIO{connection}
	go qh.EventHandler.ServeConn(quicio, &emodel.HubInfo{projectId, nodeId})
}

func (qh *QuicHandle) EventWriteLoop(hi hubio.CloudHubIO, info *emodel.HubInfo, stop chan ExitCode) {
	fmt.Printf("kexun QuicHandle EventWriteLoop")
	qh.EventHandler.eventWriteLoop(hi, info, stop)
}