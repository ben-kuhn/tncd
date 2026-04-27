#!/usr/bin/env python3
"""
AGWPE Client Emulator for testing.
Connects to AGWPE server and sends test frames.
"""

import argparse
import asyncio
import logging
import struct
import sys

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

AGWPE_HEADER_FORMAT = '<BBBBBBBB10s10sII'
AGWPE_HEADER_SIZE = 36


def create_agwpe_frame(port, datakind, call_from, call_to, data=b''):
    """Create an AGWPE frame."""
    return struct.pack(
        AGWPE_HEADER_FORMAT,
        port, 0, 0, 0, datakind, 0, 0, 0,
        call_from.encode().ljust(10, b'\x00'),
        call_to.encode().ljust(10, b'\x00'),
        len(data), 0
    ) + data


class AGWPEClientEmulator:
    def __init__(self, host='localhost', port=8000):
        self.host = host
        self.port = port
        self.reader = None
        self.writer = None
    
    async def connect(self):
        self.reader, self.writer = await asyncio.open_connection(
            self.host, self.port
        )
        logger.info(f"Connected to AGWPE server at {self.host}:{self.port}")
    
    async def send_unproto(self, from_call, to_call, data):
        """Send an UNPROTO frame."""
        frame = create_agwpe_frame(0, ord(b'M'), from_call, to_call, data)
        self.writer.write(frame)
        await self.writer.drain()
        logger.info(f"Sent UNPROTO: {from_call} -> {to_call}: {data}")
    
    async def send_connected(self, from_call, to_call, data):
        """Send a CONNECTED data frame."""
        frame = create_agwpe_frame(0, ord(b'D'), from_call, to_call, data)
        self.writer.write(frame)
        await self.writer.drain()
        logger.info(f"Sent CONNECTED: {from_call} -> {to_call}: {data}")
    
    async def send_connect(self, from_call, to_call):
        """Initiate a connection."""
        frame = create_agwpe_frame(0, ord(b'C'), from_call, to_call, b'')
        self.writer.write(frame)
        await self.writer.drain()
        logger.info(f"Sent CONNECT: {from_call} -> {to_call}")
    
    async def send_disconnect(self, from_call, to_call):
        """Disconnect."""
        frame = create_agwpe_frame(0, ord(b'd'), from_call, to_call, b'')
        self.writer.write(frame)
        await self.writer.drain()
        logger.info(f"Sent DISCONNECT: {from_call} -> {to_call}")
    
    async def send_i_frame(self, from_call, to_call, data):
        """Send an I-frame (information frame with sequence numbers)."""
        frame = create_agwpe_frame(0, ord(b'I'), from_call, to_call, data)
        self.writer.write(frame)
        await self.writer.drain()
        logger.info(f"Sent I-frame: {from_call} -> {to_call}: {data}")
    
    async def send_supervisory(self, from_call, to_call, data):
        """Send an S-frame (supervisory - RR, RNR, REJ)."""
        frame = create_agwpe_frame(0, ord(b'S'), from_call, to_call, data)
        self.writer.write(frame)
        await self.writer.drain()
        logger.info(f"Sent S-frame: {from_call} -> {to_call}")
    
    async def get_version(self):
        """Get version from server."""
        frame = create_agwpe_frame(0, ord(b'R'), 'CLIENT', 'AGWPE')
        self.writer.write(frame)
        await self.writer.drain()
        logger.info("Sent version request")
    
    async def receive(self, timeout=5.0):
        """Receive a frame."""
        try:
            header = await asyncio.wait_for(self.reader.readexactly(AGWPE_HEADER_SIZE), timeout)
            port, res1, res2, res3, datakind, res4, pid, res5, call_from, call_to, data_len, user = struct.unpack(
                AGWPE_HEADER_FORMAT, header
            )
            if data_len > 0:
                data = await asyncio.wait_for(self.reader.readexactly(data_len), timeout)
            else:
                data = b''
            
            kind = chr(datakind) if 32 <= datakind < 127 else f'\\x{datakind:02x}'
            logger.info(f"Received frame type={kind} from={call_from.decode().strip()} to={call_to.decode().strip()}")
            return {
                'type': kind,
                'from': call_from.decode().strip(),
                'to': call_to.decode().strip(),
                'data': data
            }
        except asyncio.TimeoutError:
            return None
    
    async def close(self):
        if self.writer:
            self.writer.close()
            await self.writer.wait_closed()
        logger.info("Closed connection")


async def test_roundtrip(host='localhost', port=8000):
    """Test roundtrip through the bridge - ALL frame types."""
    client = AGWPEClientEmulator(host, port)
    await client.connect()
    
    try:
        # Control frame tests
        await client.get_version()
        response = await client.receive()
        if response:
            logger.info(f"✓ Version response: {response['data']}")
        
        await asyncio.sleep(0.2)
        
        # Data frame tests - UNPROTO
        await client.send_unproto('TEST1', 'APRS', b'UNPROTO test')
        response = await client.receive(timeout=2.0)
        if response:
            logger.info(f"✓ UNPROTO response")
        
        await asyncio.sleep(0.2)
        
        # Connected data
        await client.send_connected('TEST1', 'APRS', b'CONNECTED test')
        response = await client.receive(timeout=2.0)
        if response:
            logger.info(f"✓ CONNECTED response")
        
        await asyncio.sleep(0.2)
        
        # Connect request
        await client.send_connect('TEST1', 'APRS')
        response = await client.receive(timeout=2.0)
        if response:
            logger.info(f"✓ CONNECT response")
        
        await asyncio.sleep(0.2)
        
        # Disconnect request
        await client.send_disconnect('TEST1', 'APRS')
        response = await client.receive(timeout=2.0)
        if response:
            logger.info(f"✓ DISCONNECT response")
        
        await asyncio.sleep(0.2)
        
        # I-frame (information)
        await client.send_i_frame('TEST1', 'APRS', b'I-frame data')
        response = await client.receive(timeout=2.0)
        if response:
            logger.info(f"✓ I-frame response")
        
        await asyncio.sleep(0.2)
        
        # S-frame (supervisory)
        await client.send_supervisory('TEST1', 'APRS', b'\x05\x00')  # RR frame
        response = await client.receive(timeout=2.0)
        if response:
            logger.info(f"✓ S-frame response")
        
        logger.info("=== All frame types tested ===")
        
    finally:
        await client.close()


def main():
    parser = argparse.ArgumentParser(description='AGWPE Client Emulator')
    parser.add_argument('--host', default='localhost')
    parser.add_argument('--port', type=int, default=8000)
    parser.add_argument('--repeat', type=int, default=1)
    args = parser.parse_args()
    
    async def run_tests():
        for i in range(args.repeat):
            logger.info(f"=== Test iteration {i+1} ===")
            await test_roundtrip(args.host, args.port)
            if i < args.repeat - 1:
                await asyncio.sleep(0.5)
    
    asyncio.run(run_tests())


if __name__ == '__main__':
    main()
